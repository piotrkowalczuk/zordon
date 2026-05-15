package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/control"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// discoverChain returns every Alphasfile from the leaf up to (and including)
// $HOME, root-first, with the optional global ~/.zordon/Alphasfile
// prepended. The last element is the invocation (leaf) Alphasfile.
//
// When zordon is run from <X>/.zordon/worktrees/<name>/, walkUp naturally
// climbs to <X>/Alphasfile and adopts it as the leaf — the worktree shares
// the project's file; only its Invocation (state dir, hash) differs.
func discoverChain() (chain []string, invocationFile string, err error) {
	invocationFile, err = walkUp()
	if err != nil {
		return nil, "", err
	}
	invocationFile, err = filepath.Abs(invocationFile)
	if err != nil {
		return nil, "", err
	}

	home, _ := os.UserHomeDir()

	var found []string
	dir := filepath.Dir(invocationFile)
	for {
		cand := filepath.Join(dir, "Alphasfile")
		if st, e := os.Stat(cand); e == nil && !st.IsDir() {
			found = append(found, cand)
		}
		if home != "" && dir == home {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	chain = found

	if home != "" {
		global := filepath.Join(home, ".zordon", "Alphasfile")
		if st, e := os.Stat(global); e == nil && !st.IsDir() && !slices.Contains(chain, global) {
			chain = append([]string{global}, chain...)
		}
	}
	return chain, invocationFile, nil
}

// level is one Alphasfile in the chain plus its resolved invocation identity
// and (if running) the live alpha state.
type level struct {
	afPath       string
	isInvocation bool
	inv          *invocation.Invocation
	parentCtx    *alphasfile.ParentContext
	state        *protocol.StateInfo // nil ⇒ not running
}

// resolveChain walks the chain top-down WITHOUT spawning anything, building
// each level's Invocation from the resolved services of running ancestors.
// The leaf's Invocation is derived from CWD (so a run from a worktree dir
// gets that worktree's identity); parents are always "main", rooted at
// their own directory.
func resolveChain(ctx context.Context) ([]*level, error) {
	chain, invFile, err := discoverChain()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	var accumulated []*alphasfile.Service
	out := make([]*level, 0, len(chain))
	for _, afPath := range chain {
		isInv := afPath == invFile
		raw, err := os.ReadFile(afPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", afPath, err)
		}
		parentJSON, err := json.Marshal(accumulated)
		if err != nil {
			return nil, fmt.Errorf("marshal parent ctx: %w", err)
		}
		var inv *invocation.Invocation
		if isInv {
			inv, err = invocation.New(cwd, raw, parentJSON)
		} else {
			inv, err = invocation.NewAt(filepath.Dir(afPath), raw, parentJSON)
		}
		if err != nil {
			return nil, err
		}
		var pctx *alphasfile.ParentContext
		if len(accumulated) > 0 {
			pctx = alphasfile.NewParentContext(accumulated)
		}
		var st *protocol.StateInfo
		if resp, e := control.Roundtrip(ctx, inv.SocketPath(), &protocol.Request{Op: protocol.OpState}); e == nil && resp != nil && resp.State != nil {
			st = resp.State
		} else {
			// Alpha not running; fallback to static evaluation.
			if af, err := alphasfile.Open(afPath, inv, pctx); err == nil {
				svcs := make([]*alphasfile.Service, 0, len(af.All()))
				for _, s := range af.All() {
					svcs = append(svcs, s)
				}
				st = &protocol.StateInfo{Services: svcs}
			}
		}
		out = append(out, &level{afPath: afPath, isInvocation: isInv, inv: inv, parentCtx: pctx, state: st})
		if st != nil {
			accumulated = append(accumulated, st.Services...)
		}
	}
	return out, nil
}

func runStart(ctx context.Context, log *zlog.Logger, alphaBin, alphaLog string, timeout time.Duration, failfast, verbose bool) error {
	log.Warn("zordon", "Rangers, you must act swiftly, the development environment is in grave danger!")

	chain, invFile, err := discoverChain()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if len(chain) > 1 {
		log.Info("zordon", "federation chain (%d):", len(chain))
		for _, p := range chain {
			marker := ""
			if p == invFile {
				marker = " (invocation)"
			}
			log.Info("zordon", "  - %s%s", p, marker)
		}
	}

	// flocks acquired top-down (consistent order ⇒ no deadlock), held until
	// the whole chain is handled.
	var unlocks []func()
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()

	var accumulated []*alphasfile.Service
	var dotenvChain []string // file-level dotenv of every ancestor, root-first

	for _, afPath := range chain {
		isInvocation := afPath == invFile
		parentDotenv := append([]string{}, dotenvChain...)

		raw, err := os.ReadFile(afPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", afPath, err)
		}
		parentJSON, err := json.Marshal(accumulated)
		if err != nil {
			return fmt.Errorf("marshal parent ctx: %w", err)
		}
		var inv *invocation.Invocation
		if isInvocation {
			inv, err = invocation.New(cwd, raw, parentJSON)
		} else {
			inv, err = invocation.NewAt(filepath.Dir(afPath), raw, parentJSON)
		}
		if err != nil {
			return err
		}

		unlock, err := control.Lock(inv.StateDir)
		if err != nil {
			return err
		}
		unlocks = append(unlocks, unlock)

		sock := inv.SocketPath()

		var parentCtx *alphasfile.ParentContext
		if len(accumulated) > 0 {
			parentCtx = alphasfile.NewParentContext(accumulated)
		}

		var st *protocol.StateInfo
		if resp, e := control.Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpState}); e == nil && resp != nil && resp.State != nil {
			st = resp.State
		}

		switch {
		case st != nil && !isInvocation && st.ConfigHash == inv.Hash:
			log.Info("zordon", "%s [%s] up-to-date (alpha pid=%d), reusing", afPath, inv.Hash, st.PID)
			accumulated = append(accumulated, st.Services...)
			if st.Dotenv != "" {
				dotenvChain = append(dotenvChain, st.Dotenv)
			}
			continue

		case st != nil:
			reason := "restart requested"
			if !isInvocation {
				reason = "drift detected (config changed since alpha started)"
			}
			log.Info("zordon", "%s [%s]: %s, restarting alpha pid=%d", afPath, inv.Hash, reason, st.PID)
			if _, e := control.Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpShutdown}); e != nil {
				log.Error("zordon", "shutdown %s: %v", afPath, e)
			}
			if err := waitSocketGone(ctx, sock, timeout); err != nil {
				return fmt.Errorf("%s: waiting for old alpha to exit: %w", afPath, err)
			}
		}

		af, err := alphasfile.Open(afPath, inv, parentCtx)
		if err != nil {
			return fmt.Errorf("%s: %w", afPath, err)
		}

		levelLog := inv.AlphaLogPath()
		if isInvocation && alphaLog != "" {
			levelLog = alphaLog
		}
		if err := os.MkdirAll(filepath.Dir(levelLog), 0o755); err != nil {
			return fmt.Errorf("mkdir state dir: %w", err)
		}
		// The socket lives in inv.TmpDir ($TMPDIR/zordon-<hash>); alpha
		// can't bind into a missing directory.
		if err := os.MkdirAll(inv.TmpDir, 0o755); err != nil {
			return fmt.Errorf("mkdir tmp dir: %w", err)
		}

		ctxLevel, cancel := context.WithTimeout(ctx, timeout)
		if err := spawnAlpha(alphaBin, levelLog, sock, timeout, verbose, log, inv.Env); err != nil {
			cancel()
			return fmt.Errorf("%s: %w", afPath, err)
		}
		if err := control.WaitListening(ctxLevel, sock); err != nil {
			cancel()
			return fmt.Errorf("%s: waiting for alpha socket: %w", afPath, err)
		}
		if err := pushConfigure(ctxLevel, log, sock, afPath, inv.Hash, parentDotenv, af, failfast); err != nil {
			cancel()
			return fmt.Errorf("%s: %w", afPath, err)
		}
		cancel()
		if af.Dotenv != "" {
			dotenvChain = append(dotenvChain, af.Dotenv)
		}

		// Privileged hooks are NOT run here — `zordon start` stays
		// non-interactive. Run `zordon sudo` to apply them across the chain.

		accumulated = append(accumulated, af.All()...)
	}
	return nil
}

// runSudo applies the idempotent privileged hooks for every running level
// in the chain (steps pulled from each alpha's live state so snippets carry
// the ports services actually bound to).
func runSudo(ctx context.Context, log *zlog.Logger) error {
	levels, err := resolveChain(ctx)
	if err != nil {
		return err
	}
	any := false
	for _, lv := range levels {
		if lv.state == nil || len(lv.state.Services) == 0 {
			if lv.state == nil {
				log.Info("zordon", "%s: alpha not running, skipping sudo", lv.afPath)
			}
			continue
		}
		marker := ""
		if lv.isInvocation {
			marker = " (invocation)"
		}
		log.Info("zordon", "sudo hooks for %s%s", lv.afPath, marker)
		any = true
		runSudoSteps(lv.state.Services, log)
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

// waitSocketGone blocks until nothing answers on sock (old alpha exited) or
// the timeout elapses.
func waitSocketGone(ctx context.Context, sock string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay := 20 * time.Millisecond
	for {
		c, err := control.Dial(sock, 100*time.Millisecond)
		if err != nil {
			return nil
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
