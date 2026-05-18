package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/barrier"
	"github.com/piotrkowalczuk/zordon/internal/control"
	"github.com/piotrkowalczuk/zordon/internal/lifecycle"
	"github.com/piotrkowalczuk/zordon/internal/probe"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/source"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

func main() {
	rootFlags := ff.NewFlagSet("alpha")
	rootCmd := &ff.Command{
		Name:  "alpha",
		Usage: "alpha [FLAGS] <SUBCOMMAND>",
		Flags: rootFlags,
	}

	runFlags := ff.NewFlagSet("run").SetParent(rootFlags)
	logFile := runFlags.StringLong("log-file", "/tmp/alpha.log", "path for log output")
	sockPath := runFlags.StringLong("socket", "", "control socket path (required; usually injected by zordon)")
	stabilization := runFlags.DurationLong("stabilization", 1*time.Second, "how long a service must stay alive after spawn to be considered ready")
	shutdownGrace := runFlags.DurationLong("shutdown-grace", 2*time.Second, "time given to children to exit on SIGTERM before SIGKILL")
	runCmd := &ff.Command{
		Name:      "run",
		Usage:     "alpha run --socket <path> [--log-file <path>]",
		ShortHelp: "run the alpha supervisor; signals readiness on $ZORDON_READY_FD if set",
		Flags:     runFlags,
		Exec: func(ctx context.Context, args []string) error {
			return runAlpha(ctx, *sockPath, *logFile, *stabilization, *shutdownGrace)
		},
	}
	rootCmd.Subcommands = append(rootCmd.Subcommands, runCmd)

	err := rootCmd.ParseAndRun(context.Background(), os.Args[1:])
	switch {
	case errors.Is(err, ff.ErrHelp), errors.Is(err, ff.ErrNoExec):
		fmt.Fprintln(os.Stderr, ffhelp.Command(rootCmd))
		os.Exit(0)
	case err != nil:
		fmt.Fprintf(os.Stderr, "alpha: %v\n", err)
		os.Exit(1)
	}
}

type alphaState struct {
	mu             sync.RWMutex
	startedAt      time.Time
	alphasfilePath string
	fsHash         string   // filesystem-location identity (== inv.FsHash)
	cfgHash        string   // manifest identity (Alphasfile+parent ctx); drives drift detection
	parentDotenv   []string // file-level dotenv paths inherited from federation parents
	agentMode      bool     // configured via `zordon --agent`: apply agent{} env overlay
	config         *alphasfile.Alphasfile
	services       map[string]*serviceCtx   // keyed by service name
	provisions     map[string]*provisionCtx // keyed by provision ID ("service.<tc>.<svc>.runtime.provision.<name>")
	readiness      map[string]string        // service name -> readiness state ("probing", "ready", "failed")
	files          []string                 // absolute paths of file{} outputs to unlink on shutdown

	// shutdownCh is closed exactly once to signal whole-alpha shutdown.
	// Each service supervisor selects on it; the runAlpha main goroutine
	// selects on it; everything composes via channels. requestShutdown()
	// is the only writer.
	shutdownCh   chan struct{}
	shutdownOnce sync.Once

	// handlerDones: each accepted-conn goroutine appends a fresh
	// chan struct{} on entry and closes it on exit. drainedCh waits for
	// them all. Channel-uniform alternative to sync.WaitGroup so it
	// composes with select / context timeouts in shutdown sequencing.
	handlersMu   sync.Mutex
	handlerDones []chan struct{}
}

// barrierTarget bundles the two channels alpha hands to any waiter on a
// barrier ref: target fires when the entity reaches the requested state,
// fail fires when the entity terminates in a state that means the target
// will never be reached. Waiters select on both so neither deadlock nor
// silent skip is possible.
type barrierTarget struct {
	target *barrier.Barrier
	fail   *barrier.Barrier
}

// resolveBarrier parses a canonical ref ("service.<tc>.<svc>@<state>" or
// "service.<tc>.<svc>.runtime.provision.<name>@<state>") and returns the
// concrete barriers behind it. Returns nil target if the entity isn't
// known (typo, parent-federation, dropped service).
func (s *alphaState) resolveBarrier(ref string) (*barrierTarget, error) {
	at := strings.LastIndexByte(ref, '@')
	if at < 0 {
		return nil, fmt.Errorf("barrier ref %q has no @state suffix", ref)
	}
	entityID, state := ref[:at], lifecycle.State(ref[at+1:])
	// Provision ref: service.<tc>.<svc>.runtime.provision.<name>
	if idx := strings.Index(entityID, ".runtime.provision."); idx >= 0 {
		s.mu.RLock()
		pc := s.provisions[entityID]
		s.mu.RUnlock()
		if pc == nil {
			return nil, fmt.Errorf("unknown provision %q", entityID)
		}
		t := pc.Barrier(state)
		if t == nil {
			return nil, fmt.Errorf("provision %q has no %q state", entityID, state)
		}
		return &barrierTarget{target: t, fail: pc.TerminalFailure()}, nil
	}
	// Service ref: service.<tc>.<name>
	parts := strings.SplitN(entityID, ".", 3)
	if len(parts) != 3 || parts[0] != "service" {
		return nil, fmt.Errorf("bad barrier entity ID %q", entityID)
	}
	svcName := parts[2]
	s.mu.RLock()
	sc := s.services[svcName]
	s.mu.RUnlock()
	if sc == nil {
		return nil, fmt.Errorf("unknown service %q", entityID)
	}
	t := sc.Barrier(state)
	if t == nil {
		return nil, fmt.Errorf("service %q has no %q state", entityID, state)
	}
	return &barrierTarget{target: t, fail: sc.TerminalFailure()}, nil
}

func (s *alphaState) requestShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdownCh) })
}

func (s *alphaState) registerHandler() chan struct{} {
	done := make(chan struct{})
	s.handlersMu.Lock()
	s.handlerDones = append(s.handlerDones, done)
	s.handlersMu.Unlock()
	return done
}

// drainedCh returns a channel that closes once every registered handler's
// done channel is closed. Snapshot under lock so concurrent registrations
// don't races with the drain.
func (s *alphaState) drainedCh() <-chan struct{} {
	s.handlersMu.Lock()
	snapshot := append([]chan struct{}{}, s.handlerDones...)
	s.handlersMu.Unlock()
	out := make(chan struct{})
	go func() {
		for _, c := range snapshot {
			<-c
		}
		close(out)
	}()
	return out
}

// serviceCtx is one running service plus the channels that drive its
// lifecycle. The supervisor goroutine is the sole owner of cmd.Wait —
// no other goroutine calls it — so the "exec: Wait was already called"
// race that the old two-watchers design had can't happen.
//
//	stopCh closed   → "stop just this service" (reconfigure path)
//	shutdownCh      → "stop all services" (failfast / signal)
//	cmd self-exit   → supervisor handles it (failfast trigger if `ready`)
//	done closed     → supervisor has finished; safe to read exitErr
//
// lifecycle holds the scheduled→running→{ready,failed}→stopped DAG. Reaches
// at the natural transition points; status barriers are exposed to other
// entities (provisions, cross-service waiters) via the barrierLookup.
type serviceCtx struct {
	name      string
	toolchain string
	cmd       *exec.Cmd
	stopCh    chan struct{}
	stopOnce  sync.Once
	done      chan struct{}

	lifecycle *lifecycle.Instance
	// doneBar is the synthetic "outcome decided" barrier (Any(ready,
	// failed)). It lives outside the lifecycle DAG because no single set
	// of predecessors can model "either of these happened".
	doneBar *barrier.Barrier

	// exitErr is the result of cmd.Wait. Written by supervisor before
	// closing done; the close is the happens-before edge for any reader
	// that has already received <-done.
	exitErr error
}

func newServiceCtx(name, toolchain string, cmd *exec.Cmd) *serviceCtx {
	sc := &serviceCtx{
		name:      name,
		toolchain: toolchain,
		cmd:       cmd,
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
		lifecycle: lifecycle.NewInstance(alphasfile.ServiceLifecycle),
		doneBar:   barrier.New(),
	}
	barrier.Any(sc.doneBar,
		sc.lifecycle.Barrier("ready"),
		sc.lifecycle.Barrier(alphasfile.ServiceTerminalFailure))
	sc.lifecycle.Reach("scheduled")
	return sc
}

func (sc *serviceCtx) requestStop() {
	sc.stopOnce.Do(func() { close(sc.stopCh) })
}

// provisionCtx is one bringup action — same channel/lifecycle shape as
// serviceCtx but with a transient-task vocabulary (scheduled / running /
// success / failure) and bringup-blocking semantics governed by Detached.
//
// A goroutine waits on the resolved barrier deps, optionally runs the
// idempotent check, runs cmd, then verify. The process gets a stable
// stopCh and the global state.shutdownCh just like services, so shutdown
// converges on the same mechanism.
type provisionCtx struct {
	id        string // canonical "service.<tc>.<svc>.runtime.provision.<name>"
	serviceID string // parent service identity
	step      *alphasfile.ProvisionStep

	stopCh   chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	lifecycle *lifecycle.Instance
	doneBar   *barrier.Barrier // Any(success, failure)
}

func newProvisionCtx(serviceID string, step *alphasfile.ProvisionStep) *provisionCtx {
	pc := &provisionCtx{
		id:        serviceID + ".runtime.provision." + step.Name,
		serviceID: serviceID,
		step:      step,
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
		lifecycle: lifecycle.NewInstance(alphasfile.ProvisionLifecycle),
		doneBar:   barrier.New(),
	}
	barrier.Any(pc.doneBar,
		pc.lifecycle.Barrier("success"),
		pc.lifecycle.Barrier(alphasfile.ProvisionTerminalFailure))
	return pc
}

func (pc *provisionCtx) requestStop() {
	pc.stopOnce.Do(func() { close(pc.stopCh) })
}

func (pc *provisionCtx) Barrier(s lifecycle.State) *barrier.Barrier {
	if s == alphasfile.SyntheticStateDone {
		return pc.doneBar
	}
	return pc.lifecycle.Barrier(s)
}

func (pc *provisionCtx) TerminalFailure() *barrier.Barrier {
	return pc.lifecycle.Barrier(alphasfile.ProvisionTerminalFailure)
}

// runProvision is the per-provision goroutine: own barrier lifecycle,
// waits on deps (implicit parent.ready + explicit `after` refs), runs
// check (skip cmd if check=0), runs cmd, runs verify (best effort).
// Reach success/failure terminal, close done. Failure under failfast on
// a non-detached provision triggers global shutdown.
func runProvision(pc *provisionCtx, state *alphaState, parent *serviceCtx, grace time.Duration, stream *safeEncoder, log *zlog.Logger, failfast bool) {
	defer close(pc.done)
	defer state.removeProvision(pc.id)

	pc.lifecycle.Reach("scheduled")

	type dep struct {
		ref    string
		target *barrier.Barrier
		fail   *barrier.Barrier
	}
	// Implicit: every provision waits for its parent service's ready.
	// Without this, provisions with no `after` would race the service's
	// own bringup; the implicit dep makes the common case "do this once
	// the service is up" the default.
	deps := []dep{{
		ref:    "service." + parent.toolchain + "." + parent.name + "@ready",
		target: parent.Barrier("ready"),
		fail:   parent.TerminalFailure(),
	}}
	for _, ref := range pc.step.After {
		bt, err := state.resolveBarrier(ref)
		if err != nil {
			log.Error("alpha", "provision[%s]: %v", pc.id, err)
			pc.lifecycle.Reach("failure")
			return
		}
		deps = append(deps, dep{ref: ref, target: bt.target, fail: bt.fail})
	}

	for _, d := range deps {
		select {
		case <-d.target.Wait():
		case <-d.fail.Wait():
			log.Error("alpha", "provision[%s]: dep %s reached terminal failure; skipping", pc.id, d.ref)
			pc.lifecycle.Reach("failure")
			return
		case <-pc.stopCh:
			pc.lifecycle.Reach("failure")
			return
		case <-state.shutdownCh:
			pc.lifecycle.Reach("failure")
			return
		}
	}

	pc.lifecycle.Reach("running")
	log.Info("alpha", "provision[%s]: running", pc.id)

	// Per-provision env = process env + step.Env overlay. We don't
	// reach for the parent service's RunEnv here — provisions are
	// orchestration, not the service itself; if you need parent vars,
	// reference them via interpolation in cmd.
	env := os.Environ()
	for k, v := range pc.step.Env {
		env = append(env, k+"="+v)
	}

	streamKind := "provision:" + pc.step.Name
	// Idempotency check: if it passes, skip the heavyweight cmd.
	if strings.TrimSpace(pc.step.Check) != "" {
		if err := runShell(pc, state, grace, pc.step.Check, env, parent.name, streamKind+":check", stream, log); err == nil {
			log.Info("alpha", "provision[%s]: check passed, skipping cmd", pc.id)
			pc.lifecycle.Reach("success")
			return
		}
	}

	if err := runShell(pc, state, grace, pc.step.Cmd, env, parent.name, streamKind+":cmd", stream, log); err != nil {
		log.Error("alpha", "provision[%s]: cmd failed: %v", pc.id, err)
		pc.lifecycle.Reach("failure")
		if failfast && !pc.step.Detached {
			state.requestShutdown()
		}
		return
	}

	if strings.TrimSpace(pc.step.Verify) != "" {
		if err := runShell(pc, state, grace, pc.step.Verify, env, parent.name, streamKind+":verify", stream, log); err != nil {
			log.Error("alpha", "provision[%s]: verify failed (best-effort, not fatal): %v", pc.id, err)
		}
	}

	log.Info("alpha", "provision[%s]: success", pc.id)
	pc.lifecycle.Reach("success")
}

// runShell executes a single shell snippet (check / cmd / verify) with
// the provision's env, streams its stdout+stderr through the safe
// encoder, and reacts to pc.stopCh / state.shutdownCh by SIGTERM →
// grace → SIGKILL. Returns the cmd.Wait() error (nil on exit 0).
func runShell(pc *provisionCtx, state *alphaState, grace time.Duration, snippet string, env []string, svcName, label string, stream *safeEncoder, log *zlog.Logger) error {
	cmd := exec.Command("/bin/sh", "-c", snippet)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var streamWg sync.WaitGroup
	streamWg.Add(2)
	go func() { defer streamWg.Done(); streamLines(svcName, label, stdout, stream, log) }()
	go func() { defer streamWg.Done(); streamLines(svcName, label, stderr, stream, log) }()

	cmdDone := make(chan struct{})
	var cmdErr error
	go func() {
		cmdErr = cmd.Wait()
		close(cmdDone)
	}()

	select {
	case <-cmdDone:
		streamWg.Wait()
		return cmdErr
	case <-pc.stopCh:
	case <-state.shutdownCh:
	}
	// External cancel — SIGTERM, grace, SIGKILL.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-cmdDone:
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		<-cmdDone
	}
	streamWg.Wait()
	return fmt.Errorf("cancelled")
}

// Barrier returns the barrier for an HCL-visible state. "done" is the
// synthetic Any(ready, failed); other states delegate to the lifecycle
// instance.
func (sc *serviceCtx) Barrier(s lifecycle.State) *barrier.Barrier {
	if s == alphasfile.SyntheticStateDone {
		return sc.doneBar
	}
	return sc.lifecycle.Barrier(s)
}

// TerminalFailure returns the barrier alpha pairs with any waiter on
// this service so the waiter unblocks on terminal failure instead of
// deadlocking on a status that won't be reached.
func (sc *serviceCtx) TerminalFailure() *barrier.Barrier {
	return sc.lifecycle.Barrier(alphasfile.ServiceTerminalFailure)
}

// supervise owns cmd.Wait for the lifetime of the service. It waits for
// whichever comes first — external stop or self-exit — reaches the
// matching lifecycle state, then reaps the process cleanly.
// close(sc.done) is deferred so any goroutine selecting on <-sc.done is
// unblocked exactly once, no matter which path was taken.
func (sc *serviceCtx) supervise(state *alphaState, grace time.Duration, failfast bool, log *zlog.Logger) {
	defer close(sc.done)
	defer state.removeService(sc.name)

	waitErr := make(chan error, 1)
	go func() { waitErr <- sc.cmd.Wait() }()

	var selfExit bool
	select {
	case sc.exitErr = <-waitErr:
		selfExit = true
	case <-sc.stopCh:
	case <-state.shutdownCh:
	}

	if selfExit {
		if sc.exitErr != nil {
			log.Error("alpha", "%s exited: %v", sc.name, sc.exitErr)
		} else {
			log.Info("alpha", "%s exited cleanly", sc.name)
		}
		if sc.lifecycle.Reached("ready") {
			sc.lifecycle.Reach("stopped")
			// Only "exited after ready" warrants triggering global
			// failfast; "exited before ready" is bringup's failure that
			// the EventServiceFail path already surfaces.
			if failfast {
				log.Info("alpha", "failfast: %s exited after ready, requesting alpha shutdown", sc.name)
				state.requestShutdown()
			}
		} else {
			sc.lifecycle.Reach("failed")
		}
		return
	}

	// External stop — SIGTERM, then SIGKILL on grace.
	log.Info("alpha", "SIGTERM %s pid=%d", sc.name, sc.cmd.Process.Pid)
	_ = sc.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case sc.exitErr = <-waitErr:
		log.Info("alpha", "%s exited after SIGTERM", sc.name)
	case <-time.After(grace):
		log.Info("alpha", "SIGKILL %s pid=%d (grace expired)", sc.name, sc.cmd.Process.Pid)
		_ = sc.cmd.Process.Kill()
		sc.exitErr = <-waitErr
	}
	if sc.lifecycle.Reached("ready") {
		sc.lifecycle.Reach("stopped")
	} else {
		sc.lifecycle.Reach("failed")
	}
}

func (s *alphaState) configure(path, fsHash, cfgHash string, parentDotenv []string, agent bool, af *alphasfile.Alphasfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alphasfilePath = path
	s.fsHash = fsHash
	s.cfgHash = cfgHash
	s.parentDotenv = parentDotenv
	s.agentMode = agent
	s.config = af
	s.readiness = make(map[string]string)
}

func (s *alphaState) addFile(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files = append(s.files, p)
}

func (s *alphaState) addService(sc *serviceCtx) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.services == nil {
		s.services = make(map[string]*serviceCtx)
	}
	s.services[sc.name] = sc
}

func (s *alphaState) addProvision(pc *provisionCtx) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provisions == nil {
		s.provisions = make(map[string]*provisionCtx)
	}
	s.provisions[pc.id] = pc
}

func (s *alphaState) removeProvision(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.provisions, id)
}

func (s *alphaState) setReadiness(name, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readiness == nil {
		s.readiness = make(map[string]string)
	}
	s.readiness[name] = state
}

func (s *alphaState) removeService(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.services, name)
	delete(s.readiness, name)
}

func (s *alphaState) snapshot() *protocol.StateInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info := &protocol.StateInfo{
		PID:            os.Getpid(),
		AlphasfilePath: s.alphasfilePath,
		StartedAt:      s.startedAt.Format(time.RFC3339),
		FsHash:         s.fsHash,
		CfgHash:        s.cfgHash,
	}
	if s.config != nil {
		info.Services = s.config.All()
		info.Dotenv = s.config.Dotenv
	}
	for name, sc := range s.services {
		pid := 0
		if sc.cmd.Process != nil {
			pid = sc.cmd.Process.Pid
		}
		info.Running = append(info.Running, protocol.ServiceStatus{
			Name:      name,
			PID:       pid,
			Readiness: s.readiness[name],
		})
	}
	return info
}

// shutdownAll closes every supervisor's stopCh (services AND provisions)
// and waits for their done channels. Each supervisor handles its own
// SIGTERM/grace/SIGKILL logic, so this function carries no timer of its
// own. Files generated by materializeFiles are removed at the end.
//
// Provisions get the same teardown semantics as services because they
// own subprocesses too — detached or not, they need a clean shutdown
// path when alpha is going away.
func (s *alphaState) shutdownAll(log *zlog.Logger) {
	s.mu.Lock()
	services := s.services
	provisions := s.provisions
	s.services = nil
	s.provisions = nil
	files := s.files
	s.files = nil
	s.mu.Unlock()

	for _, sc := range services {
		sc.requestStop()
	}
	for _, pc := range provisions {
		pc.requestStop()
	}
	for _, sc := range services {
		<-sc.done
	}
	for _, pc := range provisions {
		<-pc.done
	}

	for _, p := range files {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Error("alpha", "unlink %s: %v", p, err)
		} else {
			log.Info("alpha", "removed file %s", p)
		}
	}
}

// stopServices stops only the named services, leaving every other service
// untouched. Used on reconfigure so a changed service restarts without
// disturbing the ones whose code/manifest didn't change. Generated files
// are left in place (cleaned only on full shutdownAll). The supervisors
// of the affected services log their own SIGTERM/SIGKILL lifecycle.
//
// Provisions belonging to the stopped services get the same treatment —
// otherwise a detached provision launched by the previous configuration
// would keep running past its parent's death, and a fresh configure
// would register a new ctx with the same ID, leaking the old one.
func (s *alphaState) stopServices(names map[string]bool) {
	s.mu.Lock()
	pickedSvcs := make([]*serviceCtx, 0, len(names))
	for name := range names {
		if sc := s.services[name]; sc != nil {
			pickedSvcs = append(pickedSvcs, sc)
			delete(s.services, name)
		}
	}
	var pickedProvs []*provisionCtx
	for id, pc := range s.provisions {
		for _, sc := range pickedSvcs {
			prefix := "service." + sc.toolchain + "." + sc.name + ".runtime.provision."
			if strings.HasPrefix(id, prefix) {
				pickedProvs = append(pickedProvs, pc)
				delete(s.provisions, id)
				break
			}
		}
	}
	s.mu.Unlock()

	for _, sc := range pickedSvcs {
		sc.requestStop()
	}
	for _, pc := range pickedProvs {
		pc.requestStop()
	}
	for _, sc := range pickedSvcs {
		<-sc.done
	}
	for _, pc := range pickedProvs {
		<-pc.done
	}
}

// materializeFiles writes generated file{} blocks to disk atomically (write
// to a temp file in the destination directory, then rename). Returns on the
// first error so failing config doesn't leave half a tree behind.
func materializeFiles(files []*alphasfile.File, state *alphaState, log *zlog.Logger) error {
	for _, f := range files {
		if f.Path == "" {
			return fmt.Errorf("file %q has empty path", f.Name)
		}
		dir := filepath.Dir(f.Path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
		tmp, err := os.CreateTemp(dir, ".zordon-"+filepath.Base(f.Path)+".*")
		if err != nil {
			return fmt.Errorf("temp file in %s: %w", dir, err)
		}
		if _, err := tmp.WriteString(f.Body); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return fmt.Errorf("close %s: %w", tmp.Name(), err)
		}
		if err := os.Rename(tmp.Name(), f.Path); err != nil {
			os.Remove(tmp.Name())
			return fmt.Errorf("rename %s -> %s: %w", tmp.Name(), f.Path, err)
		}
		state.addFile(f.Path)
		log.Info("alpha", "wrote file %s (%d bytes)", f.Path, len(f.Body))
	}
	return nil
}

// safeEncoder serializes writes from multiple goroutines and silently drops
// events once the underlying connection has gone away.
type safeEncoder struct {
	mu     sync.Mutex
	enc    *protocol.Encoder
	closed atomic.Bool
}

func newSafeEncoder(enc *protocol.Encoder) *safeEncoder {
	return &safeEncoder{enc: enc}
}

func (e *safeEncoder) Send(ev *protocol.Event) {
	if e.closed.Load() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() {
		return
	}
	if err := e.enc.Write(ev); err != nil {
		e.closed.Store(true)
	}
}

func (e *safeEncoder) Close() { e.closed.Store(true) }

// IsClosed exposes the closed flag so callers can skip allocations that
// would only be thrown away by Send.
func (e *safeEncoder) IsClosed() bool { return e.closed.Load() }

type bringupConfig struct {
	stabilization time.Duration
	shutdownGrace time.Duration
}

func runAlpha(_ context.Context, sockPath, logPath string, stabilization, shutdownGrace time.Duration) error {
	if sockPath == "" {
		return errors.New("--socket is required")
	}

	logF, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logF.Close()

	log := zlog.New(logF, false)
	log.Info("alpha", "starting pid=%d socket=%s", os.Getpid(), sockPath)

	ln, err := control.Listen(sockPath)
	if err != nil {
		return fmt.Errorf("control listen: %w", err)
	}
	defer ln.Close()
	log.Info("alpha", "listening on %s", sockPath)

	state := &alphaState{
		startedAt:  time.Now(),
		shutdownCh: make(chan struct{}),
	}
	cfg := bringupConfig{stabilization: stabilization, shutdownGrace: shutdownGrace}

	if err := signalReady(); err != nil {
		log.Error("alpha", "ready signal: %v", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	acceptDone := make(chan struct{})
	go func() {
		acceptLoop(ln, state, cfg, log)
		close(acceptDone)
	}()

	select {
	case sig := <-sigCh:
		log.Info("alpha", "received %s, shutting down children", sig)
		state.requestShutdown()
	case <-state.shutdownCh:
		log.Info("alpha", "shutdown requested via control socket")
	}
	// Ordered shutdown so the conn that triggered shutdown can flush its
	// final EventError/EventDone and close cleanly:
	//   1. close listener  -> no new connections accepted
	//   2. wait acceptLoop -> in-flight registerHandler() calls observed
	//   3. shutdownAll     -> close stopCh / wait done for every service
	//   4. drain handlers  -> defer conn.Close() in handleConn fires before
	//                         exit, so zordon sees the final event then EOF.
	//                         Bounded by shutdownGrace so a hung handler
	//                         (zordon stopped reading) can't pin alpha.
	_ = ln.Close()
	<-acceptDone
	state.shutdownAll(log)
	select {
	case <-state.drainedCh():
	case <-time.After(shutdownGrace):
		log.Error("alpha", "handlers did not drain within %s, exiting anyway", shutdownGrace)
	}
	return nil
}

func signalReady() error {
	raw := os.Getenv("ZORDON_READY_FD")
	if raw == "" {
		return nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return fmt.Errorf("bad ZORDON_READY_FD=%q", raw)
	}
	f := os.NewFile(uintptr(fd), "zordon-ready")
	if f == nil {
		return fmt.Errorf("fd %d not open", fd)
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, "READY=1")
	return err
}

func acceptLoop(ln net.Listener, state *alphaState, cfg bringupConfig, log *zlog.Logger) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Error("alpha", "accept: %v", err)
			return
		}
		// Register the handler's done channel BEFORE spawning so the
		// shutdown-side drainedCh() snapshot can't observe an empty list
		// between Accept and the goroutine's first instruction.
		done := state.registerHandler()
		go func() {
			defer close(done)
			handleConn(conn, state, cfg, log)
		}()
	}
}

func handleConn(conn net.Conn, state *alphaState, cfg bringupConfig, log *zlog.Logger) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	dec := protocol.NewDecoder(conn)
	enc := protocol.NewEncoder(conn)

	var req protocol.Request
	if err := dec.Read(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return
		}
		log.Error("alpha", "read req: %v", err)
		_ = enc.Write(&protocol.Response{Error: err.Error()})
		return
	}

	switch req.Op {
	case protocol.OpConfigure:
		_ = conn.SetDeadline(time.Time{})
		handleConfigure(&req, state, cfg, enc, log)
	case protocol.OpState:
		_ = enc.Write(&protocol.Response{OK: true, State: state.snapshot()})
	case protocol.OpShutdown:
		log.Info("alpha", "shutdown op received")
		state.requestShutdown()
		_ = enc.Write(&protocol.Response{OK: true})
	default:
		_ = enc.Write(&protocol.Response{Error: fmt.Sprintf("unknown op: %q", req.Op)})
	}
}

func handleConfigure(req *protocol.Request, state *alphaState, cfg bringupConfig, enc *protocol.Encoder, log *zlog.Logger) {
	if req.Configure == nil || req.Configure.Alphasfile == nil {
		_ = enc.Write(&protocol.Event{Kind: protocol.EventError, Error: "configure: missing alphasfile"})
		return
	}

	failfast := req.Configure.Failfast
	newConfig := req.Configure.Alphasfile

	// Determine services to stop: those running but missing or changed in newConfig
	toStop := make(map[string]bool)
	state.mu.RLock()
	oldConfig := state.config
	running := make(map[string]bool)
	for name := range state.services {
		running[name] = true
	}
	state.mu.RUnlock()

	if oldConfig != nil {
		newSvcs := make(map[string]*alphasfile.Service)
		for _, s := range newConfig.All() {
			newSvcs[s.Name()] = s
		}

		for _, oldSvc := range oldConfig.All() {
			name := oldSvc.Name()
			if !running[name] {
				continue
			}
			newSvc, exists := newSvcs[name]
			if !exists || fmt.Sprintf("%v", oldSvc) != fmt.Sprintf("%v", newSvc) {
				toStop[name] = true
			}
		}
	}

	if len(toStop) > 0 {
		state.stopServices(toStop)
	}

	state.configure(req.Configure.AlphasfilePath, req.Configure.FsHash, req.Configure.CfgHash, req.Configure.ParentDotenv, req.Configure.Agent, newConfig)
	services := newConfig.All()
	files := newConfig.AllFiles()
	log.Info("alpha", "configured from %s (%d services, %d files, failfast=%v), starting bringup",
		req.Configure.AlphasfilePath, len(services), len(files), failfast)

	stream := newSafeEncoder(enc)
	defer stream.Close()

	if err := materializeFiles(files, state, log); err != nil {
		log.Error("alpha", "materialize files: %v", err)
		stream.Send(&protocol.Event{Kind: protocol.EventError, Error: "file: " + err.Error()})
		return
	}

	// Track services that were just brought up — provisions only spawn
	// for those (provisions of preserved services already ran in their
	// previous bringup; rerunning would violate idempotency expectations).
	var brought []*alphasfile.Service
	for _, svc := range services {
		if running[svc.Name()] && !toStop[svc.Name()] {
			continue // Already running and unchanged
		}
		ok := bringupService(svc, state, cfg, stream, log, failfast)
		if !ok && failfast {
			log.Info("alpha", "failfast: %s did not come up, aborting remaining bringup", svc.Name())
			state.shutdownAll(log)
			stream.Send(&protocol.Event{Kind: protocol.EventError, Error: fmt.Sprintf("failfast: %s failed during bringup", svc.Name())})
			state.requestShutdown()
			return
		}
		if ok {
			brought = append(brought, svc)
		}
	}

	// Launch provisions for every newly-brought-up service. Each one
	// gets its own goroutine that waits on deps then runs check/cmd/
	// verify. We collect refs to the non-detached ones so EventDone can
	// block on their completion.
	type prov struct {
		pc       *provisionCtx
		detached bool
	}
	var provs []prov
	for _, svc := range brought {
		if svc.Runtime == nil || len(svc.Runtime.Provision) == 0 {
			continue
		}
		state.mu.RLock()
		parent := state.services[svc.Name()]
		state.mu.RUnlock()
		if parent == nil {
			continue
		}
		for _, step := range svc.Runtime.Provision {
			pc := newProvisionCtx("service."+svc.Toolchain+"."+svc.Name(), step)
			state.addProvision(pc)
			go runProvision(pc, state, parent, cfg.shutdownGrace, stream, log, failfast)
			provs = append(provs, prov{pc: pc, detached: step.Detached})
		}
	}

	// Wait for non-detached provisions before declaring bringup complete.
	// Detached ones keep running in the background; alpha shutdown will
	// reap them along with the services.
	for _, p := range provs {
		if p.detached {
			continue
		}
		select {
		case <-p.pc.doneBar.Wait():
			if !p.pc.lifecycle.Reached("success") && failfast {
				log.Info("alpha", "failfast: provision %s did not succeed, aborting", p.pc.id)
				state.shutdownAll(log)
				stream.Send(&protocol.Event{Kind: protocol.EventError, Error: fmt.Sprintf("failfast: provision %s failed", p.pc.id)})
				state.requestShutdown()
				return
			}
		case <-state.shutdownCh:
			// Someone else triggered shutdown; bringup is done.
			return
		}
	}

	stream.Send(&protocol.Event{Kind: protocol.EventDone})
	log.Info("alpha", "bringup complete")
}

func bringupService(svc *alphasfile.Service, state *alphaState, cfg bringupConfig, stream *safeEncoder, log *zlog.Logger, failfast bool) bool {
	name := svc.Name()
	log.Info("alpha", "bringup service=%s toolchain=%s", name, svc.Toolchain)
	stream.Send(&protocol.Event{Kind: protocol.EventServiceStart, Service: name})

	state.mu.RLock()
	agent := state.agentMode
	state.mu.RUnlock()

	repoDir, err := prepare(svc, name, agent, stream, log)
	if err != nil {
		log.Error("alpha", "prepare %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		return false
	}

	// env precedence (low→high): process env → federation parents'
	// file-level dotenv (root-first) → this Alphasfile's file-level
	// dotenv → this service's dotenv → this service's env block.
	state.mu.RLock()
	envFiles := append([]string{}, state.parentDotenv...)
	if state.config != nil && state.config.Dotenv != "" {
		envFiles = append(envFiles, state.config.Dotenv)
	}
	state.mu.RUnlock()
	if svc.Runtime != nil && svc.Runtime.Dotenv != "" {
		envFiles = append(envFiles, svc.Runtime.Dotenv)
	}

	cmd, err := buildCmd(svc, repoDir, envFiles, agent)
	if err != nil {
		log.Error("alpha", "build cmd %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		return false
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// rt.Log + rt.Log.TTY are populated for every service by the resolver,
	// so reading them here is safe.
	stdout, stderr, ttyCleanup, err := attachStdio(cmd, *svc.Runtime.Log.TTY)
	if err != nil {
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		return false
	}

	if err := cmd.Start(); err != nil {
		ttyCleanup()
		log.Error("alpha", "start %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		return false
	}
	// Parent's copy of the PTY slave is no longer needed after fork+exec;
	// the child has its own descriptor. Closing here lets EOF on the master
	// propagate when the child exits.
	ttyCleanup()

	sc := newServiceCtx(name, svc.Toolchain, cmd)
	state.addService(sc)
	state.setReadiness(name, "probing")
	sc.lifecycle.Reach("running")
	log.Info("alpha", "started %s pid=%d argv=%v", name, cmd.Process.Pid, cmd.Args)

	go streamLines(name, "stdout", stdout, stream, log)
	go streamLines(name, "stderr", stderr, stream, log)
	go sc.supervise(state, cfg.shutdownGrace, failfast, log)

	// Probe (or stabilization) runs in its own goroutine so we can race
	// it against sc.done — if the child crashes before becoming ready,
	// we abandon the probe immediately.
	readyCh := make(chan error, 1)
	probeCtx, cancelProbe := context.WithCancel(context.Background())
	defer cancelProbe()
	go func() { readyCh <- waitReady(probeCtx, svc, cfg.stabilization, log) }()

	select {
	case err := <-readyCh:
		if err != nil {
			state.setReadiness(name, "failed")
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: "readiness: " + err.Error()})
			// Tell supervisor to terminate — we won't be running this service.
			sc.requestStop()
			<-sc.done
			return false
		}
		sc.lifecycle.Reach("ready")
		state.setReadiness(name, "ready")
		stream.Send(&protocol.Event{Kind: protocol.EventServiceReady, Service: name})
		return true
	case <-sc.done:
		// Supervisor reaped the cmd before bringup completed.
		state.setReadiness(name, "failed")
		msg := "exited before ready"
		if sc.exitErr != nil {
			msg = sc.exitErr.Error()
		}
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: msg})
		return false
	}
}

// waitReady runs the readiness probe (or stabilization timer if none) and
// returns nil on ready, or an error describing why probing failed.
// Composable: callers run this in a goroutine and select its result
// channel against other lifecycle signals.
func waitReady(ctx context.Context, svc *alphasfile.Service, stabilization time.Duration, log *zlog.Logger) error {
	p := readinessOf(svc)
	if p == nil {
		select {
		case <-time.After(stabilization):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	log.Info("alpha", "probing %s readiness", svc.Name())
	return p.Wait(ctx, func(ok bool, reason string) {
		if ok {
			log.Info("alpha", "probe ok %s (%s)", svc.Name(), reason)
		} else {
			log.Info("alpha", "probe miss %s (%s)", svc.Name(), reason)
		}
	})
}

func readinessOf(svc *alphasfile.Service) *probe.Probe {
	if svc == nil || svc.Runtime == nil {
		return nil
	}
	return svc.Runtime.Readiness
}

// attachStdio wires up stdout/stderr for a child process. With useTTY=false
// both are anonymous pipes (the default). With useTTY=true stdout is given
// a PTY slave so the child sees isatty(1)==true and picks line-buffering
// itself; stderr stays as a pipe so the stream-attribution survives.
//
// cleanup must be called after cmd.Start() to drop the parent's copy of the
// slave end. Safe to call even when no PTY was allocated (no-op).
func attachStdio(cmd *exec.Cmd, useTTY bool) (stdout, stderr io.ReadCloser, cleanup func(), err error) {
	cleanup = func() {}
	stderr, err = cmd.StderrPipe()
	if err != nil {
		return nil, nil, cleanup, err
	}
	if !useTTY {
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return nil, nil, cleanup, err
		}
		return stdout, stderr, cleanup, nil
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, nil, cleanup, fmt.Errorf("pty open: %w", err)
	}
	cmd.Stdout = tty
	// stderr remains the pipe from StderrPipe above.
	cleanup = func() { _ = tty.Close() }
	return ptmx, stderr, cleanup, nil
}

func streamLines(name, kind string, r io.Reader, stream *safeEncoder, log *zlog.Logger) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		// PTYs default to ONLCR (LF→CRLF on output); strip the trailing CR
		// rather than fiddle with termios so the same path handles pipes
		// (no CR) and PTYs uniformly.
		line := strings.TrimRight(s.Text(), "\r")
		log.Service(name, kind, line)
		// Skip the event allocation entirely when zordon has detached:
		// the marshal would be thrown away, and per-line allocations
		// dominate GC pressure for talkative services.
		if !stream.IsClosed() {
			stream.Send(&protocol.Event{
				Kind:    protocol.EventLog,
				Service: name,
				Stream:  kind,
				Line:    line,
			})
		}
	}
}

// parseDotenv reads a .env file: `KEY=VALUE` per line, `#` comments and
// blank lines ignored, optional surrounding single/double quotes stripped,
// optional leading `export `. Missing file ⇒ no vars (not an error: the
// file may legitimately be absent).
func parseDotenv(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		ln = strings.TrimPrefix(ln, "export ")
		k, v, ok := strings.Cut(ln, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out = append(out, k+"="+v)
	}
	return out
}

// serviceEnv builds the child process environment: the alpha process env
// as a base, then each dotenv file in order, then the explicit env map —
// later entries override earlier ones.
func serviceEnv(envFiles []string, env map[string]string) []string {
	merged := map[string]string{}
	put := func(kv string) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	for _, kv := range os.Environ() {
		put(kv)
	}
	for _, f := range envFiles {
		for _, kv := range parseDotenv(f) {
			put(kv)
		}
	}
	for k, v := range env {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// mergeEnv overlays maps left→right (later entries win); nil maps skipped.
func mergeEnv(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// phaseEnv is the explicit env map for one phase: the service-wide
// `env {}` base, then the phase overlay (build/runtime), then the
// `agent {}` overlay when alpha runs in --agent mode (each later layer
// overrides earlier keys).
func phaseEnv(svc *alphasfile.Service, phase map[string]string, agent bool) map[string]string {
	if svc.Runtime == nil {
		return nil
	}
	var ag map[string]string
	if agent {
		ag = svc.Runtime.AgentEnv
	}
	return mergeEnv(svc.Runtime.Env, phase, ag)
}

func buildCmd(svc *alphasfile.Service, checkout string, envFiles []string, agent bool) (*exec.Cmd, error) {
	name := svc.Name()
	if name == "" {
		return nil, errors.New("service has no name")
	}
	binDir := ""
	if svc.Runtime != nil {
		binDir = svc.Runtime.BinDir
	}
	var cmd *exec.Cmd
	switch {
	case svc.Runtime != nil && len(svc.Runtime.Command) > 0:
		// Explicit argv (subcommand-driven binaries / built artifacts).
		argv := svc.Runtime.Command
		cmd = exec.Command(argv[0], argv[1:]...)
	case binDir != "" && svc.Buildable():
		// Every service is built by zordon (git/src checkout or crate
		// cargo-install) into the out-of-tree bin dir; run it from there.
		// cwd = source checkout (when there is one) so relative config
		// paths resolve.
		cmd = exec.Command(filepath.Join(binDir, name), svc.Flags()...)
	default:
		cmd = exec.Command(name, svc.Flags()...)
	}
	if checkout != "" {
		cmd.Dir = checkout
	}
	var runEnv map[string]string
	if svc.Runtime != nil {
		runEnv = phaseEnv(svc, svc.Runtime.RunEnv, agent)
	}
	if len(envFiles) > 0 || len(runEnv) > 0 {
		cmd.Env = serviceEnv(envFiles, runEnv)
	}
	return cmd, nil
}

// defaultBuild is the per-toolchain build run inside a fresh checkout when
// the service doesn't set `build = "..."`. For Go it honors `package`
// (the main package within the repo, default ".") so a repo whose main
// lives in cmd/foo needs no explicit build string. Output goes to the
// out-of-tree bin dir so a `dir` primary's worktree stays clean.
func defaultBuild(svc *alphasfile.Service, name, binDir string) string {
	out := filepath.Join(binDir, name)
	root := filepath.Dir(binDir) // binDir = <stateDir>/bin
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(root)))
	rustCache := filepath.Join(projectRoot, ".zordon", "cache", "rust", "target")

	// Use-only: install the dependency's binary into fs::bin, no checkout.
	if svc.UseOnly() {
		switch svc.Toolchain {
		case alphasfile.ToolchainGo:
			// go install drops the binary (named after the package's last
			// path element) into GOBIN — point that at fs::bin.
			return fmt.Sprintf("GOBIN=%q go install %s", binDir, svc.Package.Install)
		case alphasfile.ToolchainRust:
			opts := ""
			if len(svc.Package.Features) > 0 {
				opts += fmt.Sprintf(" --features %q", strings.Join(svc.Package.Features, ","))
			}
			if v := strings.TrimSpace(svc.Package.Version); v != "" {
				opts += fmt.Sprintf(" --version %q", v)
			}
			if b := strings.TrimSpace(svc.Package.Bin); b != "" {
				opts += fmt.Sprintf(" --bin %q", b)
			}
			// crates are immutable ⇒ no --force (reuse if already installed).
			return fmt.Sprintf("CARGO_TARGET_DIR=%q cargo install %q --root %q%s --locked",
				rustCache, svc.Package.Install, root, opts)
		}
		return ""
	}

	switch svc.Toolchain {
	case alphasfile.ToolchainGo:
		// cwd is the anchored workdir (checkout + Alphasfile offset); exe is
		// the build target relative to it (default ".").
		pkg := "."
		if svc.Package != nil && strings.TrimSpace(svc.Package.Exe) != "" {
			pkg = svc.Package.Exe
		}
		return fmt.Sprintf("go build -o %q %s", out, pkg)
	case alphasfile.ToolchainRust:
		// Worktree rust: `cargo install --path` from the checkout into
		// <stateDir>/bin (== binDir). Stable CARGO_TARGET_DIR keeps
		// compilation incremental across runs; --force so code edits land.
		opts := ""
		if svc.Package != nil && len(svc.Package.Features) > 0 {
			opts += fmt.Sprintf(" --features %q", strings.Join(svc.Package.Features, ","))
		}
		if svc.Package != nil && strings.TrimSpace(svc.Package.Bin) != "" {
			opts += fmt.Sprintf(" --bin %q", svc.Package.Bin)
		}
		path := "."
		if svc.Package != nil && strings.TrimSpace(svc.Package.Exe) != "" {
			path = svc.Package.Exe
		}
		return fmt.Sprintf("CARGO_TARGET_DIR=%q cargo install --path %q --root %q%s --locked --force",
			rustCache, path, root, opts)
	case alphasfile.ToolchainRuby:
		// `--path` was removed in Bundler 2.x — write the path into the
		// per-checkout .bundle/config first (so `bundle exec` at runtime
		// finds the gems too) and then install.
		return "bundle config set --local path vendor/bundle && bundle install"
	}
	return ""
}

// prepare materializes a per-invocation git worktree for the service and
// runs its build step in that checkout. Worktree-able = has a git or dir
// primary; crate/prebuilt services skip this and run from $PATH. Output is
// streamed back to zordon under stream="prepare".
//
// Returns the checkout dir (empty for non-worktree services) so the caller
// can set exec.Cmd.Dir.
// prepare produces the service's binary into the out-of-tree bin dir and
// returns the directory to use as the run cwd:
//   - git/src: materialize a per-invocation worktree, build in it, cwd =
//     checkout.
//   - crate:   `cargo install <crate>` (no checkout), cwd = "".
// Every service must be one of these (the resolver rejects sourceless
// services); there is no prebuilt-$PATH path.
func prepare(svc *alphasfile.Service, name string, agent bool, stream *safeEncoder, log *zlog.Logger) (string, error) {
	if svc.Package == nil || !svc.Buildable() {
		return "", nil
	}
	runner := newPrepareRunner(name, stream, log)
	ctx := context.Background()

	binDir := ""
	if svc.Runtime != nil {
		binDir = svc.Runtime.BinDir
	}
	if binDir != "" {
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir bin dir: %w", err)
		}
	}

	dest := "" // run cwd; "" for crate (no checkout)
	switch {
	case svc.Package.InPlace:
		// src-only in the "main" worktree: use the live src tree as-is —
		// no git worktree add, no HEAD reset. Uncommitted edits just work.
		if svc.Runtime != nil {
			dest = svc.Runtime.Dir
		}
		if dest == "" {
			return "", fmt.Errorf("in-place service %q has no resolved src dir", name)
		}
		log.Info("alpha", "prepare %s: in place %s (no worktree)", name, dest)
	case svc.Worktreeable():
		if svc.Runtime != nil {
			dest = svc.Runtime.Dir
		}
		if dest == "" {
			return "", fmt.Errorf("worktree-able service %q has no resolved checkout dir", name)
		}
		var worktree *source.Worktree
		if svc.Package.Worktree != nil {
			worktree = &source.Worktree{Sparse: svc.Package.Worktree.Sparse}
		}
		p, err := source.NewPrimary(svc.Package.Git, svc.Package.Src, svc.Ref(), worktree)
		if err != nil {
			return "", err
		}
		log.Info("alpha", "prepare %s: ensuring primary (%s)", name, p.Kind)
		if err := p.Ensure(ctx, runner); err != nil {
			return "", fmt.Errorf("ensure primary: %w", err)
		}
		wt := os.Getenv("ZORDON_WORKTREE")
		if wt == "" {
			wt = "main"
		}
		// Branch is per (worktree, service): a monorepo where several
		// services share one primary repo would otherwise all want
		// `zordon/<wt>` and the 2nd `git worktree add` would fail with
		// "branch already checked out". Slashes are valid in ref names.
		branch := "zordon/" + wt + "/" + name
		log.Info("alpha", "prepare %s: worktree -> %s (branch %s)", name, dest, branch)
		if err := p.AddWorktree(ctx, dest, branch, runner); err != nil {
			return "", fmt.Errorf("git worktree: %w", err)
		}
	default:
		log.Info("alpha", "prepare %s: use-only %s (install)", name, svc.Package.Install)
	}

	// Explicit `build { cmd = [...] }` is exec'd as argv (no shell). With
	// no build block, the toolchain default runs via /bin/sh (it relies
	// on shell: env-prefixed `GOBIN=… go install`, `&&`, etc).
	var c *exec.Cmd
	if bc := svc.BuildCmd(); len(bc) > 0 {
		log.Info("alpha", "prepare %s: build (%v)", name, bc)
		c = exec.Command(bc[0], bc[1:]...)
	} else if def := strings.TrimSpace(defaultBuild(svc, name, binDir)); def != "" {
		log.Info("alpha", "prepare %s: build (%s)", name, def)
		c = exec.Command("/bin/sh", "-c", def)
	}
	if c != nil {
		c.Dir = dest // "" ⇒ alpha's cwd; fine for `cargo install <crate>`
		if svc.Runtime != nil {
			if be := phaseEnv(svc, svc.Runtime.BuildEnv, agent); len(be) > 0 {
				c.Env = serviceEnv(nil, be)
			}
		}
		if err := runner(ctx, c); err != nil {
			return "", fmt.Errorf("build: %w", err)
		}
	}
	return dest, nil
}

// newPrepareRunner builds a source.Runner that pipes exec.Cmd output through
// the per-service log stream and waits for the command to exit.
func newPrepareRunner(name string, stream *safeEncoder, log *zlog.Logger) source.Runner {
	return func(ctx context.Context, c *exec.Cmd) error {
		stdout, err := c.StdoutPipe()
		if err != nil {
			return err
		}
		stderr, err := c.StderrPipe()
		if err != nil {
			return err
		}
		if err := c.Start(); err != nil {
			return err
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); streamLines(name, "prepare", stdout, stream, log) }()
		go func() { defer wg.Done(); streamLines(name, "prepare", stderr, stream, log) }()
		wg.Wait()
		return c.Wait()
	}
}
