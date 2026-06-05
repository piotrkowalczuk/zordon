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
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// discoverChain returns every Alphasfile from the leaf up to (and
// including) ZORDON_HOME's parent directory, root-first, with the
// optional global <ZORDON_HOME>/Alphasfile prepended. The last
// element is the invocation (leaf) Alphasfile.
//
// Why bound at filepath.Dir(ZORDON_HOME) rather than $HOME:
//
//	ZORDON_HOME defines the "zordon-world" — where artifacts live and
//	how far zordon's view extends. In production with the default
//	ZORDON_HOME=~/.zordon, the bound is ~/ — identical to the
//	historical $HOME semantic, so user-facing behavior is unchanged.
//	When ZORDON_HOME is redirected (test harness, sandboxed runner,
//	multi-tenant CI), the bound moves with it: an agent running
//	zordon with ZORDON_HOME=/sandbox/zordon won't traverse past
//	/sandbox/ during federation discovery, which is the same scope
//	the toolchain cache and registry already live in.
//
// When zordon is run from <X>/.zordon/worktrees/<name>/, walkUp
// naturally climbs to <X>/Alphasfile and adopts it as the leaf —
// the worktree shares the project's file; only its Invocation
// (state dir, hash) differs.
func discoverChain(zordonHome string) (chain []string, invocationFile string, err error) {
	invocationFile, err = walkUp()
	if err != nil {
		return nil, "", err
	}
	invocationFile, err = filepath.Abs(invocationFile)
	if err != nil {
		return nil, "", err
	}

	// Bound = parent of zordon home. Walk-up stops AT this dir
	// (inclusive — we still scan the bound itself for an Alphasfile).
	// Empty zordonHome means no override available, so don't enforce
	// a stop here — let parent==dir filesystem-root catch handle it.
	var bound string
	if zordonHome != "" {
		bound = filepath.Dir(zordonHome)
	}

	var found []string
	dir := filepath.Dir(invocationFile)
	for {
		cand := filepath.Join(dir, "Alphasfile")
		if st, e := zfs.Stat(cand); e == nil && !st.IsDir() {
			found = append(found, cand)
		}
		if bound != "" && dir == bound {
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

	if zordonHome != "" {
		global := filepath.Join(zordonHome, "Alphasfile")
		if st, e := zfs.Stat(global); e == nil && !st.IsDir() && !slices.Contains(chain, global) {
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
	inv          *invocation.InvocationState
	parentCtx    *alphasfile.ParentContext
	cfgHash      string              // manifest identity: invocation.ConfigHash(bytes, parentJSON)
	state        *protocol.StateInfo // nil ⇒ not running
}

// resolveChain walks the chain top-down WITHOUT spawning anything, building
// each level's Invocation from the resolved services of running ancestors.
// The leaf's Invocation is derived from CWD (so a run from a worktree dir
// gets that worktree's identity); parents are always "main", rooted at
// their own directory.
func resolveChain(ctx context.Context, zordonHome string, testCfg alphasfile.TestConfig) ([]*level, error) {
	return walkChain(zordonHome, func(lv *level) (*protocol.StateInfo, error) {
		// Prefer the running alpha (carries live PIDs / readiness); fall
		// back to static evaluation. A static-eval error is tolerated
		// here — `status` should still print "alpha not running" rather
		// than refuse the whole report just because one level can't be
		// re-evaluated from disk.
		if resp, e := control.Roundtrip(ctx, lv.inv.SocketPath(), &protocol.Request{Op: protocol.OpState}); e == nil && resp != nil && resp.State != nil {
			return resp.State, nil
		}
		af, err := alphasfile.Open(lv.afPath, lv.inv, lv.parentCtx, lv.cfgHash, testCfg)
		if err != nil {
			return nil, nil //nolint:nilerr // intentional: see comment above
		}
		return stateFromAlphasfile(af), nil
	})
}

// stateFromAlphasfile projects a resolved Alphasfile into the wire-shape
// StateInfo used as the static-evaluation fallback for federation walks.
// No PIDs / Running — only the static facets (services, toolchain pins,
// sysenv whitelist, file-level dotenv).
func stateFromAlphasfile(af *alphasfile.Alphasfile) *protocol.StateInfo {
	return &protocol.StateInfo{
		Services:  append([]*alphasfile.Service(nil), af.All()...),
		Toolchain: af.Toolchain,
		SysEnv:    af.SysEnv,
		Dotenv:    append([]string(nil), af.Dotenv...),
	}
}

// walkChain is the canonical federation-chain walk: discover root→leaf,
// build the per-level Invocation and ParentContext (accumulating
// services / toolchain / sysenv as it descends), and delegate to resolve
// for the level's StateInfo. Used by every command that needs the
// resolved chain — `status`, `sudo`, `get`, `plan`, `stop`.
//
// resolve owns the per-level policy (live alpha first vs. static-only
// vs. strict-static); a returned (nil, nil) means "no state for this
// level" — deeper levels won't see its services as parent context.
func walkChain(zordonHome string, resolve func(*level) (*protocol.StateInfo, error)) ([]*level, error) {
	fed, err := NewFederationState(zordonHome)
	if err != nil {
		return nil, err
	}

	var parents alphasfile.GlobalComputedState
	out := make([]*level, 0, len(fed.Levels()))
	for _, cl := range fed.Levels() {
		parentJSON, err := json.Marshal(parents.Services())
		if err != nil {
			return nil, fmt.Errorf("marshal parent ctx: %w", err)
		}
		lv := &level{
			afPath:       cl.Path(),
			isInvocation: cl.IsLeaf(),
			inv:          cl.Invocation(),
			parentCtx:    parents.ParentContext(),
			cfgHash:      invocation.ConfigHash(cl.Bytes(), parentJSON),
		}
		st, err := resolve(lv)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cl.Path(), err)
		}
		lv.state = st
		out = append(out, lv)
		if st != nil {
			parents.Join(adoptedBlock(st))
		}
	}
	return out, nil
}

func runStart(ctx context.Context, log *zlog.Logger, alphaBin, alphaLog string, timeout time.Duration, failfast, verbose, agent bool, picks []string, zordonHome string, testCfg alphasfile.TestConfig) error {
	log.Warn("zordon", "Rangers, you must act swiftly, the development environment is in grave danger!")

	fed, err := NewFederationState(zordonHome)
	if err != nil {
		return err
	}
	if len(fed.Levels()) > 1 {
		log.Info("zordon", "federation chain (%d):", len(fed.Levels()))
		for _, lvl := range fed.Levels() {
			marker := ""
			if lvl.IsLeaf() {
				marker = " (invocation)"
			}
			log.Info("zordon", "  - %s%s", lvl.Path(), marker)
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

	cfg := startConfig{
		alphaBin: alphaBin,
		alphaLog: alphaLog,
		timeout:  timeout,
		failfast: failfast,
		verbose:  verbose,
		agent:    agent,
	}

	var parents alphasfile.GlobalComputedState
	for _, lvl := range fed.Levels() {
		inv := lvl.Invocation()
		parentDotenv := append([]string{}, parents.Dotenv()...)
		parentJSON, err := json.Marshal(parents.Services())
		if err != nil {
			return fmt.Errorf("marshal parent ctx: %w", err)
		}
		cfgHash := invocation.ConfigHash(lvl.Bytes(), parentJSON)

		unlock, err := control.Lock(inv.StateDir)
		if err != nil {
			return err
		}
		unlocks = append(unlocks, unlock)

		// resolveLevel carries the multi-agent invariant: the leaf always
		// recomputes/restarts; a parent is reused unless its config drifted.
		af, old, reused, err := resolveLevel(ctx, lvl, cfgHash, parents.ParentContext(), testCfg)
		if err != nil {
			return fmt.Errorf("%s: %w", lvl.Path(), err)
		}
		if reused {
			log.Info("zordon", "%s [%s] up-to-date (alpha pid=%d), reusing", lvl.Path(), inv.FsHash, old.PID)
			parents.Join(adoptedBlock(old))
			continue
		}

		if lvl.IsLeaf() && len(picks) > 0 {
			filtered, err := pickServices(af.All(), picks)
			if err != nil {
				return fmt.Errorf("%s: %w", lvl.Path(), err)
			}
			log.Info("zordon", "picks=%v → bringing up %d of %d service(s)", picks, len(filtered), len(af.All()))
			af.Services = filtered
		}

		if err := reconcileAlpha(ctx, lvl, af, old, parentDotenv, cfg, log); err != nil {
			return err
		}

		// Privileged hooks are NOT run here — `zordon start` stays
		// non-interactive. Run `zordon sudo` to apply them across the chain.
		parents.Join(af.Block())
	}
	return nil
}

// runSudo applies the idempotent privileged hooks for every running level
// in the chain (steps pulled from each alpha's live state so snippets carry
// the ports services actually bound to).
func runSudo(ctx context.Context, log *zlog.Logger, zordonHome string, testCfg alphasfile.TestConfig) error {
	levels, err := resolveChain(ctx, zordonHome, testCfg)
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
