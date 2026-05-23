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
	"syscall"
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
	inv          *invocation.Invocation
	parentCtx    *alphasfile.ParentContext
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
		af, err := alphasfile.Open(lv.afPath, lv.inv, lv.parentCtx, testCfg)
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
	chain, invFile, err := discoverChain(zordonHome)
	if err != nil {
		return nil, err
	}
	cwd, err := zfs.Getwd()
	if err != nil {
		return nil, err
	}

	var accumulated []*alphasfile.Service
	accumulatedToolchain := map[string]*alphasfile.ToolchainConfig{}
	var accumulatedSysEnv []string
	out := make([]*level, 0, len(chain))
	for _, afPath := range chain {
		isInv := afPath == invFile
		raw, err := zfs.Read(afPath)
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
		if len(accumulated) > 0 || len(accumulatedToolchain) > 0 || len(accumulatedSysEnv) > 0 {
			pctx = alphasfile.NewParentContext(accumulated).WithToolchain(accumulatedToolchain).WithSysEnv(accumulatedSysEnv)
		}
		lv := &level{afPath: afPath, isInvocation: isInv, inv: inv, parentCtx: pctx}
		st, err := resolve(lv)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", afPath, err)
		}
		lv.state = st
		out = append(out, lv)
		if st != nil {
			accumulated = append(accumulated, st.Services...)
			for k, v := range st.Toolchain {
				accumulatedToolchain[k] = v
			}
			accumulatedSysEnv = appendUniqueStrings(accumulatedSysEnv, st.SysEnv)
		}
	}
	return out, nil
}

// appendUniqueStrings adds entries from extra to dst preserving order of
// first occurrence. Used to accumulate sysenv whitelists down a federation
// chain without duplicates (a key wider repeats are no-ops).
func appendUniqueStrings(dst, extra []string) []string {
	seen := make(map[string]struct{}, len(dst))
	for _, s := range dst {
		seen[s] = struct{}{}
	}
	for _, s := range extra {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		dst = append(dst, s)
	}
	return dst
}

func runStart(ctx context.Context, log *zlog.Logger, alphaBin, alphaLog string, timeout time.Duration, failfast, verbose, agent bool, zordonHome string, testCfg alphasfile.TestConfig) error {
	log.Warn("zordon", "Rangers, you must act swiftly, the development environment is in grave danger!")

	chain, invFile, err := discoverChain(zordonHome)
	if err != nil {
		return err
	}
	cwd, err := zfs.Getwd()
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
	accumulatedToolchain := map[string]*alphasfile.ToolchainConfig{}
	var accumulatedSysEnv []string
	var dotenvChain []string // file-level dotenv of every ancestor, root-first

	for _, afPath := range chain {
		isInvocation := afPath == invFile
		parentDotenv := append([]string{}, dotenvChain...)

		raw, err := zfs.Read(afPath)
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
		if len(accumulated) > 0 || len(accumulatedToolchain) > 0 || len(accumulatedSysEnv) > 0 {
			parentCtx = alphasfile.NewParentContext(accumulated).WithToolchain(accumulatedToolchain).WithSysEnv(accumulatedSysEnv)
		}

		var st *protocol.StateInfo
		if resp, e := control.Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpState}); e == nil && resp != nil && resp.State != nil {
			st = resp.State
		}

		switch {
		case st != nil && !isInvocation && st.CfgHash == inv.CfgHash:
			log.Info("zordon", "%s [%s] up-to-date (alpha pid=%d), reusing", afPath, inv.FsHash, st.PID)
			accumulated = append(accumulated, st.Services...)
			for k, v := range st.Toolchain {
				accumulatedToolchain[k] = v
			}
			accumulatedSysEnv = appendUniqueStrings(accumulatedSysEnv, st.SysEnv)
			dotenvChain = append(dotenvChain, st.Dotenv...)
			continue

		case st != nil:
			reason := "restart requested"
			if !isInvocation {
				reason = "drift detected (config changed since alpha started)"
			}
			log.Info("zordon", "%s [%s]: %s, restarting alpha pid=%d", afPath, inv.FsHash, reason, st.PID)
			if _, e := control.Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpShutdown}); e != nil {
				log.Error("zordon", "shutdown %s: %v", afPath, e)
			}
			if err := waitProcessGone(ctx, st.PID, timeout); err != nil {
				return fmt.Errorf("%s: waiting for old alpha to exit: %w", afPath, err)
			}
		}

		af, err := alphasfile.Open(afPath, inv, parentCtx, testCfg)
		if err != nil {
			return fmt.Errorf("%s: %w", afPath, err)
		}

		levelLog := inv.AlphaLogPath()
		if isInvocation && alphaLog != "" {
			levelLog = alphaLog
		}
		if err := zfs.EnsureSharedDir(filepath.Dir(levelLog)); err != nil {
			return fmt.Errorf("mkdir state dir: %w", err)
		}
		// The socket lives in inv.TmpDir ($TMPDIR/zordon-<FsHash>); alpha
		// can't bind into a missing directory.
		if err := zfs.EnsureSharedDir(inv.TmpDir); err != nil {
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
		if err := pushConfigure(ctxLevel, log, sock, afPath, inv.FsHash, inv.CfgHash, parentDotenv, af, failfast, agent); err != nil {
			cancel()
			return fmt.Errorf("%s: %w", afPath, err)
		}
		cancel()
		dotenvChain = append(dotenvChain, af.Dotenv...)

		// Privileged hooks are NOT run here — `zordon start` stays
		// non-interactive. Run `zordon sudo` to apply them across the chain.

		accumulated = append(accumulated, af.All()...)
		for k, v := range af.Toolchain {
			accumulatedToolchain[k] = v
		}
		accumulatedSysEnv = appendUniqueStrings(accumulatedSysEnv, af.SysEnv)
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

// waitProcessGone blocks until the alpha process at pid has actually
// exited (kill(pid, 0) == ESRCH), not just until its control socket
// stopped accepting. On timeout it escalates to SIGKILL on the process
// group (alpha sets Setpgid at spawn so pid == pgid, and the kill also
// reaps any tommy/service children the dying alpha didn't manage to
// clean up itself) and re-checks briefly.
//
// Why this matters: alpha unlinks its control socket as the FIRST step
// of shutdown (ln.Close on a *net.UnixListener with UnlinkOnClose), but
// the process stays alive while shutdownAll drives each service through
// SIGTERM → grace → SIGKILL. A socket-file poll therefore unblocks
// seconds before the process is gone, so spawnAlpha would launch a
// second alpha with the same --socket / --log-file argv. Both processes
// then coexist (one listening, one cleaning up) until the old one
// finishes — indefinitely if a service is wedged. waitProcessGone is
// the tight fix: PID gone ⇒ safe to spawn.
//
// A zero/negative pid is treated as "no PID known" and returns nil; the
// caller falls back to whatever next step it had planned (typically the
// control.Listen unlinkStale + bind path), preserving behavior for
// historical callers that don't yet propagate a PID.
func waitProcessGone(ctx context.Context, pid int, timeout time.Duration) error {
	if pid <= 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	delay := 20 * time.Millisecond
	for {
		if !processAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			break
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
	// Escalation: SIGKILL the pgid (covers any service/tommy the wedged
	// alpha was supervising) and give the kernel a short window to flip
	// the PID to ESRCH. Only an uninterruptible-IO straggler can survive
	// this; in that case we report the error so the caller fails loudly
	// instead of spawning a sibling.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	for i := 0; i < 20; i++ {
		if !processAlive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("alpha pid %d still alive after SIGKILL", pid)
}

// processAlive reports whether pid corresponds to a live process. EPERM
// means "exists but we can't signal it", which still counts as alive
// for our purposes (we won't spawn over a process we can see).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
