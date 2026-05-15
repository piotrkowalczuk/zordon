package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/control"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// discoverChain returns every Alphasfile from the invocation directory up to
// (and including) $HOME, ordered root-first, with the optional global
// ~/.zordon/Alphasfile prepended. The last element is the invocation
// Alphasfile — the only one zordon ever restarts unconditionally.
func discoverChain() (chain []string, invocation string, err error) {
	invocation, err = walkUp()
	if err != nil {
		return nil, "", err
	}
	invocation, err = filepath.Abs(invocation)
	if err != nil {
		return nil, "", err
	}

	home, _ := os.UserHomeDir()

	var found []string // child → parent order
	dir := filepath.Dir(invocation)
	for {
		cand := filepath.Join(dir, "Alphasfile")
		if st, e := os.Stat(cand); e == nil && !st.IsDir() {
			found = append(found, cand)
		}
		if home != "" && dir == home {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir { // hit filesystem root
			break
		}
		dir = parent
	}
	// Reverse to root-first.
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	chain = found

	if home != "" {
		global := filepath.Join(home, ".zordon", "Alphasfile")
		if st, e := os.Stat(global); e == nil && !st.IsDir() && !contains(chain, global) {
			chain = append([]string{global}, chain...)
		}
	}
	return chain, invocation, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// configHash binds an Alphasfile's source bytes to the parent context that
// fed its evaluation. A change to either (e.g. a grandparent restarting with
// new ports) flips the hash, which is how drift cascades down a chain.
func configHash(raw, parentJSON []byte) string {
	h := sha256.New()
	h.Write(raw)
	h.Write([]byte{0})
	h.Write(parentJSON)
	return hex.EncodeToString(h.Sum(nil))
}

func runStart(ctx context.Context, log *zlog.Logger, alphaBin, alphaLog string, timeout time.Duration, failfast, verbose bool) error {
	log.Warn("zordon", "Rangers, you must act swiftly, the development environment is in grave danger!")

	chain, invocation, err := discoverChain()
	if err != nil {
		return err
	}
	if len(chain) > 1 {
		log.Info("zordon", "federation chain (%d):", len(chain))
		for _, p := range chain {
			marker := ""
			if p == invocation {
				marker = " (invocation)"
			}
			log.Info("zordon", "  - %s%s", p, marker)
		}
	}

	// Acquire flocks strictly top-down (consistent global order ⇒ no
	// deadlock) and hold them all until the whole chain is handled.
	var unlocks []func()
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()

	var accumulated []*alphasfile.Service // resolved services of every parent

	for _, afPath := range chain {
		isInvocation := afPath == invocation

		stateDir, err := control.StateDir(afPath)
		if err != nil {
			return err
		}
		unlock, err := control.Lock(stateDir)
		if err != nil {
			return err
		}
		unlocks = append(unlocks, unlock)

		sock, err := control.SocketPath(afPath)
		if err != nil {
			return err
		}

		raw, err := os.ReadFile(afPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", afPath, err)
		}
		parentJSON, err := json.Marshal(accumulated)
		if err != nil {
			return fmt.Errorf("marshal parent ctx: %w", err)
		}
		hash := configHash(raw, parentJSON)

		var parentCtx *alphasfile.ParentContext
		if len(accumulated) > 0 {
			parentCtx = alphasfile.NewParentContext(accumulated)
		}

		// Probe a running alpha.
		var st *protocol.StateInfo
		if resp, e := control.Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpState}); e == nil && resp != nil && resp.State != nil {
			st = resp.State
		}

		switch {
		case st != nil && !isInvocation && st.ConfigHash == hash:
			log.Info("zordon", "%s up-to-date (alpha pid=%d), reusing", afPath, st.PID)
			accumulated = append(accumulated, st.Services...)
			continue

		case st != nil:
			reason := "restart requested"
			if !isInvocation {
				reason = "drift detected (config changed since alpha started)"
			}
			log.Info("zordon", "%s: %s, restarting alpha pid=%d", afPath, reason, st.PID)
			if _, e := control.Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpShutdown}); e != nil {
				log.Error("zordon", "shutdown %s: %v", afPath, e)
			}
			if err := waitSocketGone(ctx, sock, timeout); err != nil {
				return fmt.Errorf("%s: waiting for old alpha to exit: %w", afPath, err)
			}
		}

		// (Re)start this level.
		af, err := alphasfile.Open(afPath, stateDir, parentCtx)
		if err != nil {
			return fmt.Errorf("%s: %w", afPath, err)
		}

		levelLog := filepath.Join(stateDir, "alpha.log")
		if isInvocation && alphaLog != "" {
			levelLog = alphaLog
		}

		ctxLevel, cancel := context.WithTimeout(ctx, timeout)
		if err := spawnAlpha(alphaBin, levelLog, sock, timeout, verbose, log); err != nil {
			cancel()
			return fmt.Errorf("%s: %w", afPath, err)
		}
		if err := control.WaitListening(ctxLevel, sock); err != nil {
			cancel()
			return fmt.Errorf("%s: waiting for alpha socket: %w", afPath, err)
		}
		if err := pushConfigure(ctxLevel, log, sock, afPath, hash, af, failfast); err != nil {
			cancel()
			return fmt.Errorf("%s: %w", afPath, err)
		}
		cancel()

		// Privileged hooks are NOT run here — `zordon start` stays
		// non-interactive. Run `zordon sudo` to apply them across the chain.

		accumulated = append(accumulated, af.All()...)
	}
	return nil
}

// runSudo applies the idempotent privileged hooks for every Alphasfile in
// the chain. It pulls the resolved steps from each *running* alpha's state
// (so snippets reference the ports services are actually bound to, not a
// fresh re-evaluation that would pick new ports).
func runSudo(ctx context.Context, log *zlog.Logger) error {
	chain, invocation, err := discoverChain()
	if err != nil {
		return err
	}
	any := false
	for _, afPath := range chain {
		sock, err := control.SocketPath(afPath)
		if err != nil {
			return err
		}
		resp, err := control.Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpState})
		if err != nil {
			if control.IsNotRunning(err) {
				log.Info("zordon", "%s: alpha not running, skipping sudo", afPath)
				continue
			}
			return err
		}
		if resp.Error != "" {
			return fmt.Errorf("alpha: %s", resp.Error)
		}
		if resp.State == nil || len(resp.State.Services) == 0 {
			continue
		}
		marker := ""
		if afPath == invocation {
			marker = " (invocation)"
		}
		log.Info("zordon", "sudo hooks for %s%s", afPath, marker)
		any = true
		runSudoSteps(resp.State.Services, log)
	}
	if !any {
		return errors.New("no running alpha in the federation chain")
	}
	return nil
}

// runSudoSteps runs each service's idempotent privileged hooks. `check`
// runs WITHOUT sudo: if it exits 0 the step is already satisfied and we
// skip `apply` entirely — so no password prompt on steady state. Only when
// check fails (or is absent) does `apply` run via sudo (one prompt), wired
// to the user's terminal. Optional `verify` runs WITHOUT sudo afterwards.
// Failures are logged, not fatal.
func runSudoSteps(services []*alphasfile.Service, log *zlog.Logger) {
	sh := func(snippet string, sudo bool) error {
		var cmd *exec.Cmd
		if sudo {
			cmd = exec.Command("sudo", "/bin/sh", "-c", snippet)
		} else {
			cmd = exec.Command("/bin/sh", "-c", snippet)
		}
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stderr // keep stdout clean for structured output
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	for _, s := range services {
		if s.Runtime == nil {
			continue
		}
		for _, step := range s.Runtime.Sudo {
			if strings.TrimSpace(step.Apply) == "" {
				continue
			}
			if c := strings.TrimSpace(step.Check); c != "" {
				if err := sh(c, false); err == nil {
					log.Info("zordon", "sudo[%s/%s]: already satisfied, skipping", s.Name(), step.Name)
					continue
				}
			}
			log.Info("zordon", "sudo[%s/%s]: applying (may prompt for password)", s.Name(), step.Name)
			if err := sh(step.Apply, true); err != nil {
				log.Error("zordon", "sudo[%s/%s]: apply failed (continuing): %v", s.Name(), step.Name, err)
				continue
			}
			if v := strings.TrimSpace(step.Verify); v != "" {
				if err := sh(v, false); err != nil {
					log.Error("zordon", "sudo[%s/%s]: verify failed: %v", s.Name(), step.Name, err)
				} else {
					log.Info("zordon", "sudo[%s/%s]: verified", s.Name(), step.Name)
				}
			}
		}
	}
}

// waitSocketGone blocks until nothing answers on sock (the old alpha has
// fully exited) or the timeout elapses.
func waitSocketGone(ctx context.Context, sock string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay := 20 * time.Millisecond
	for {
		c, err := control.Dial(sock, 100*time.Millisecond)
		if err != nil {
			return nil // refused/not-there ⇒ gone
		}
		c.Close()
		if time.Now().After(deadline) {
			return errors.New("old alpha still listening")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 250*time.Millisecond {
			delay *= 2
		}
	}
}
