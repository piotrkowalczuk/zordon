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
	"github.com/piotrkowalczuk/zordon/internal/tools"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// applyToolchainEnv looks up the materialized toolchain entity for the
// service's language label and overlays its cached env (and any
// toolchain.<lang>.env user overlay) onto cmd.Env. No-op when toolchain
// isn't pinned (status quo: use whatever's on PATH from sysenv).
//
// The actual install (EnsureMise / EnsureTools / mise install runtime
// version) happens ONCE per (lang, version) on a dedicated toolchainCtx
// goroutine launched at configure time — see bringupToolchain. This
// function blocks on that goroutine reaching `ready` (typical case: it
// already has by the time a service spawn reaches here, because the
// implicit `toolchain.<lang>@ready` dep gates runtime.after).
//
// Returns an error if the toolchain entity reached terminal failure,
// so the caller can fail the service spawn loudly rather than launch
// it with an env from a broken toolchain.
func applyToolchainEnv(cmd *exec.Cmd, toolchainLang string, state *alphaState, log *zlog.Logger) error {
	_ = log // reserved for future "waited for ready" debug logs
	state.mu.RLock()
	tc := state.toolchains[toolchainLang]
	state.mu.RUnlock()
	if tc == nil {
		// No toolchain entity ⇒ Alphasfile didn't pin this lang.
		// Spawn runs with whatever cmd.Env already has from serviceEnv.
		return nil
	}
	// Block until the toolchain reaches a terminal state. In normal
	// flow the implicit runtime.after dep already unblocked this
	// service when ready fired; the wait here is the belt-and-braces
	// fallback for code paths (provisions, build cmd) that might not
	// have been gated explicitly.
	select {
	case <-tc.lifecycle.Barrier("ready").Wait():
	case <-tc.lifecycle.Barrier(alphasfile.ToolchainTerminalFailure).Wait():
		return fmt.Errorf("toolchain %s@%s: install failed (see earlier `toolchain` log lines)", tc.lang, tc.version)
	}
	cmd.Env = mergeEnvLists(cmd.Env, tc.env)
	if len(tc.userEnv) > 0 {
		cmd.Env = mergeEnvLists(cmd.Env, tc.userEnv)
	}
	return nil
}

// mergeEnvLists folds overlay into base, returning a flat KEY=VAL list
// with overlay winning on key collisions. Used to layer mise's env +
// toolchain.<lang>.env on top of what buildCmd/serviceEnv produced.
func mergeEnvLists(base []string, overlay map[string]string) []string {
	if len(overlay) == 0 {
		return base
	}
	merged := map[string]string{}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	for k, v := range overlay {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// logWriter adapts a zlog.Logger as an io.Writer so subprocess output
// (mise install / cargo install progress) lands in alpha's log under
// a recognizable source tag.
type writeLog struct {
	log *zlog.Logger
	src string
}

func (w *writeLog) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			w.log.Info(w.src, "%s", line)
		}
	}
	return len(p), nil
}

func logWriter(log *zlog.Logger, src string) io.Writer { return &writeLog{log: log, src: src} }

// killGroup sends sig to the process group led by pid. Every service /
// provision spawn sets Setpgid: true, so the process is its own group
// leader (pid == pgid) and a signal to the negative pid hits the whole
// group — the service plus any subprocesses it forked itself. Without
// this, only the group leader would receive the signal; its children
// would be reparented to init and linger as orphans.
//
// The function is best-effort: kill returns ESRCH if the group is
// already gone, which is fine because the caller's loop drains the
// done channel anyway.
func killGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

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
	services       map[string]*serviceCtx    // keyed by service name
	provisions     map[string]*provisionCtx  // keyed by provision ID ("service.<tc>.<svc>.runtime.provision.<name>")
	toolchains     map[string]*toolchainCtx  // keyed by lang ("go", "rust", "ruby")
	readiness      map[string]string         // service name -> readiness state ("probing", "ready", "failed")
	files          []string                  // absolute paths of file{} outputs to unlink on shutdown

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

	// Per-primary mutex map so two services that share a git/dir
	// primary (monorepo case) don't race in Ensure (clone) when they
	// bring up in parallel. Sequential bringup masked this; once we
	// fan out, two goroutines would both stat-then-clone the same
	// bare path.
	primaryMu    sync.Mutex
	primaryLock_ map[string]*sync.Mutex
}

func (s *alphaState) primaryLockFor(path string) *sync.Mutex {
	s.primaryMu.Lock()
	defer s.primaryMu.Unlock()
	if s.primaryLock_ == nil {
		s.primaryLock_ = map[string]*sync.Mutex{}
	}
	mu, ok := s.primaryLock_[path]
	if !ok {
		mu = &sync.Mutex{}
		s.primaryLock_[path] = mu
	}
	return mu
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

// resolveBarrier parses a canonical ref and returns the concrete
// barriers behind it. Grammar:
//
//	toolchain.<lang>@<state>                                    — toolchain barrier
//	service.<tc>.<svc>.build@<state>                            — service build barrier
//	service.<tc>.<svc>.runtime@<state>                          — service runtime barrier
//	service.<tc>.<svc>.runtime.provision.<name>@<state>          — provision barrier
//
// The path mirrors HCL block nesting (self-discoverable: see a block,
// guess the path). Returns an error for unknown entities (typo,
// dropped service, federation parent not in scope).
func (s *alphaState) resolveBarrier(ref string) (*barrierTarget, error) {
	at := strings.LastIndexByte(ref, '@')
	if at < 0 {
		return nil, fmt.Errorf("barrier ref %q has no @state suffix", ref)
	}
	entityID, state := ref[:at], lifecycle.State(ref[at+1:])
	// Toolchain ref: `toolchain.<lang>`. Cheapest to check first by
	// prefix because nothing else starts with it.
	if strings.HasPrefix(entityID, "toolchain.") {
		lang := strings.TrimPrefix(entityID, "toolchain.")
		s.mu.RLock()
		tc := s.toolchains[lang]
		s.mu.RUnlock()
		if tc == nil {
			return nil, fmt.Errorf("unknown toolchain %q (not pinned in Alphasfile.toolchain{})", entityID)
		}
		t := tc.Barrier(state)
		if t == nil {
			return nil, fmt.Errorf("toolchain %q has no %q state", entityID, state)
		}
		return &barrierTarget{target: t, fail: tc.TerminalFailure()}, nil
	}
	// Provision ref: ends with `.runtime.provision.<name>`.
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
	// Service build ref: ends with `.build` (and isn't a provision).
	// Check before `.runtime` so a future `service.X.build.subblock`
	// doesn't get misdispatched.
	if strings.HasSuffix(entityID, ".build") {
		svcID := strings.TrimSuffix(entityID, ".build")
		parts := strings.SplitN(svcID, ".", 3)
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
		t := sc.BuildBarrier(state)
		if t == nil {
			return nil, fmt.Errorf("service %q has no %q build state", entityID, state)
		}
		return &barrierTarget{target: t, fail: sc.BuildTerminalFailure()}, nil
	}
	// Service runtime ref: ends with `.runtime` (and isn't a provision).
	if strings.HasSuffix(entityID, ".runtime") {
		svcID := strings.TrimSuffix(entityID, ".runtime")
		parts := strings.SplitN(svcID, ".", 3)
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
			return nil, fmt.Errorf("service %q has no %q runtime state", entityID, state)
		}
		return &barrierTarget{target: t, fail: sc.TerminalFailure()}, nil
	}
	return nil, fmt.Errorf("bad barrier entity ID %q (expected toolchain.<lang> | service.<tc>.<n>.{build,runtime[.provision.<p>]})", entityID)
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

// toolchainCtx is one materialized language pin (e.g. ruby@3.3.10) —
// installing the interpreter via mise, installing the declared tools
// (gem install bundler, ...), then caching the resolved env so every
// service of that language can layer it onto cmd.Env in O(1) without
// re-running mise per spawn.
//
// One instance per (lang, version) declared in Alphasfile.Toolchain.
// Allocated and Reach("installing")'d in handleConfigure before any
// service goroutine sees its first `runtime.after` barrier; the
// bringupToolchain goroutine then drives EnsureMise → EnsureTools →
// MiseEnv and Reaches "ready" (or "failed") on completion.
//
// All operations are serialized per (lang, version) by tools.Acquire
// — same lock that prevented the gem-install race when applyToolchain-
// Env did this work inline.
type toolchainCtx struct {
	lang    string
	version string
	// env is the result of `mise env --json <lang>@<version>`, set
	// before Reach("ready"). Read only after waiting on the ready
	// barrier — the write happens-before that close.
	env map[string]string
	// userEnv is the Alphasfile-declared overlay (`toolchain { <lang>
	// { env = {...} } }`). Set at allocation; never mutated.
	userEnv map[string]string
	// tools is the name→version map of language-native tools to
	// install into this pinned interpreter (bundler@2.5.3, dlv@..., etc).
	tools     map[string]string
	lifecycle *lifecycle.Instance
	doneBar   *barrier.Barrier // Any(ready, failed)
}

func newToolchainCtx(lang, version string, toolsMap, userEnv map[string]string) *toolchainCtx {
	tc := &toolchainCtx{
		lang:      lang,
		version:   version,
		tools:     toolsMap,
		userEnv:   userEnv,
		lifecycle: lifecycle.NewInstance(alphasfile.ToolchainLifecycle),
		doneBar:   barrier.New(),
	}
	barrier.Any(tc.doneBar,
		tc.lifecycle.Barrier("ready"),
		tc.lifecycle.Barrier(alphasfile.ToolchainTerminalFailure))
	return tc
}

func (tc *toolchainCtx) Barrier(s lifecycle.State) *barrier.Barrier {
	if s == alphasfile.SyntheticStateDone {
		return tc.doneBar
	}
	return tc.lifecycle.Barrier(s)
}

func (tc *toolchainCtx) TerminalFailure() *barrier.Barrier {
	return tc.lifecycle.Barrier(alphasfile.ToolchainTerminalFailure)
}

// bringupToolchain runs the install side-effects for one pinned
// toolchain on a dedicated goroutine: EnsureMise (cargo install if
// first run) → tools.Acquire (per-(lang,version) flock) →
// EnsureTools (gem install bundler, etc.) → MiseEnv (read PATH/etc.
// in JSON). On success: cache env, Reach("ready"). On any error:
// Reach("failed") so service goroutines waiting on toolchain@ready
// unblock via terminal-failure pairing rather than deadlocking.
//
// Idempotent enough that a federation level booting after a parent
// has already materialized the same version will short-circuit on
// the existing installs/locks and finish in milliseconds.
func bringupToolchain(tc *toolchainCtx, log *zlog.Logger) {
	tc.lifecycle.Reach("installing")
	log.Info("alpha", "toolchain %s@%s: installing", tc.lang, tc.version)

	userHome, err := os.UserHomeDir()
	if err != nil {
		log.Error("alpha", "toolchain %s: user home: %v", tc.lang, err)
		tc.lifecycle.Reach("failed")
		return
	}
	zordonHome := filepath.Join(userHome, ".zordon")
	bin, err := tools.EnsureMise(zordonHome, logWriter(log, "mise-install"))
	if err != nil {
		log.Error("alpha", "toolchain %s: ensure mise: %v", tc.lang, err)
		tc.lifecycle.Reach("failed")
		return
	}
	// ResolveDataDir uses the user's home as the walk-up anchor —
	// alpha doesn't have a per-service cwd to pivot from at this
	// stage (we materialize toolchains BEFORE services run).
	dataDir := tools.ResolveDataDir(userHome, userHome, tc.lang, tc.version)
	release, err := tools.Acquire(dataDir, tc.lang, tc.version)
	if err != nil {
		log.Error("alpha", "toolchain %s: acquire: %v", tc.lang, err)
		tc.lifecycle.Reach("failed")
		return
	}
	defer release()

	// Tools must be installed before MiseEnv because mise env's
	// PATH lists the per-version bin dir — but the gem/bundler
	// binaries land under `installs/<lang>/<version>/lib/.../gems/...`
	// only after gem install runs.
	envWriter := logWriter(log, "mise-tool")
	if err := tools.EnsureTools(bin, dataDir, tc.lang, tc.version, tc.tools, envWriter); err != nil {
		log.Error("alpha", "toolchain %s: ensure tools: %v", tc.lang, err)
		tc.lifecycle.Reach("failed")
		return
	}
	env, err := tools.MiseEnv(bin, dataDir, tc.lang, tc.version, logWriter(log, "mise"))
	if err != nil {
		log.Error("alpha", "toolchain %s: mise env: %v", tc.lang, err)
		tc.lifecycle.Reach("failed")
		return
	}
	tc.env = env
	tc.lifecycle.Reach("ready")
	log.Info("alpha", "toolchain %s@%s: ready", tc.lang, tc.version)
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

	// build is the parallel one-shot lifecycle for the service's build
	// phase (the `build { cmd = [...] }` step, or the toolchain default
	// when absent). Exposed as `service.<tc>.<n>.build@<state>` so other
	// services sharing the source checkout can wait on success before
	// their own runtime starts (`vendor/bundle` populated, generated
	// code present, etc.) — see karafka/sidekiq waiting on bnpl.build.
	//
	// Services with no build cmd reach `success` synthetically the
	// moment the prepare step finishes (Pass-like behavior).
	build        *lifecycle.Instance
	buildDoneBar *barrier.Barrier // Any(build.success, build.failure)

	// exitErr is the result of cmd.Wait. Written by supervisor before
	// closing done; the close is the happens-before edge for any reader
	// that has already received <-done.
	exitErr error
}

func newServiceCtx(name, toolchain string) *serviceCtx {
	sc := &serviceCtx{
		name:         name,
		toolchain:    toolchain,
		stopCh:       make(chan struct{}),
		done:         make(chan struct{}),
		lifecycle:    lifecycle.NewInstance(alphasfile.ServiceLifecycle),
		doneBar:      barrier.New(),
		build:        lifecycle.NewInstance(alphasfile.BuildLifecycle),
		buildDoneBar: barrier.New(),
	}
	barrier.Any(sc.doneBar,
		sc.lifecycle.Barrier("ready"),
		sc.lifecycle.Barrier(alphasfile.ServiceTerminalFailure))
	barrier.Any(sc.buildDoneBar,
		sc.build.Barrier("success"),
		sc.build.Barrier(alphasfile.BuildTerminalFailure))
	sc.lifecycle.Reach("scheduled")
	sc.build.Reach("scheduled")
	return sc
}

// BuildBarrier returns the build-phase barrier for state s, or the
// composed done barrier when s == SyntheticStateDone.
func (sc *serviceCtx) BuildBarrier(s lifecycle.State) *barrier.Barrier {
	if s == alphasfile.SyntheticStateDone {
		return sc.buildDoneBar
	}
	return sc.build.Barrier(s)
}

// BuildTerminalFailure is the build's terminal-failure barrier, paired
// with every BuildBarrier in resolveBarrier so waiters don't deadlock
// when a build fails.
func (sc *serviceCtx) BuildTerminalFailure() *barrier.Barrier {
	return sc.build.Barrier(alphasfile.BuildTerminalFailure)
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
// waits on EXPLICIT `after` refs (no implicit parent.ready dep — the
// user is responsible for declaring `after = [self.ready, ...]` if the
// snippet needs the service operational), runs check (skip cmd if
// check=0), runs cmd, runs verify (best effort). Reach success/failure
// terminal, close done. Failure under failfast on a non-detached
// provision triggers global shutdown.
func runProvision(pc *provisionCtx, state *alphaState, parent *serviceCtx, grace time.Duration, stream *safeEncoder, log *zlog.Logger, failfast bool) {
	defer close(pc.done)
	// Same reasoning as bringupAndSupervise: keep the entry in
	// state.provisions so other entities that wait on this provision's
	// success/failure can still resolve it after the goroutine has
	// returned. Map cleanup is the next configure's or shutdown's job.

	pc.lifecycle.Reach("scheduled")

	for _, ref := range pc.step.After {
		bt, err := state.resolveBarrier(ref)
		if err != nil {
			log.Error("alpha", "provision[%s]: %v", pc.id, err)
			pc.lifecycle.Reach("failure")
			return
		}
		select {
		case <-bt.target.Wait():
		case <-bt.fail.Wait():
			log.Error("alpha", "provision[%s]: dep %s reached terminal failure; skipping", pc.id, ref)
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

	// Per-provision env precedence (low → high):
	//   sysenv-filtered process env
	//   parent service's env { } block
	//   parent service's runtime.env (if any) — keeps migrate scripts in
	//                                           sync with the daemon's vars
	//   parent service's agent.env when --agent
	//   provision step's own env { }
	//
	// Inheriting the parent service's env is what `db_url = ...` in
	// service.env needs to be visible to a `migrate` provision without
	// the user having to retype it.
	state.mu.RLock()
	var sysenv []string
	var parentSvc *alphasfile.Service
	agentMode := state.agentMode
	if state.config != nil {
		sysenv = state.config.SysEnv
		for _, s := range state.config.All() {
			if s.Name() == parent.name {
				parentSvc = s
				break
			}
		}
	}
	state.mu.RUnlock()
	env := filterEnviron(os.Environ(), sysenv)
	if parentSvc != nil {
		parentEnv := phaseEnv(parentSvc, parentSvc.Runtime.RunEnv, agentMode)
		for k, v := range parentEnv {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range pc.step.Env {
		env = append(env, k+"="+v)
	}

	streamKind := "provision:" + pc.step.Name
	// Idempotency check: if it passes, skip the heavyweight cmd.
	if strings.TrimSpace(pc.step.Check) != "" {
		if err := runShell(pc, state, grace, pc.step.Check, env, parent.name, parent.toolchain, streamKind+":check", stream, log); err == nil {
			log.Info("alpha", "provision[%s]: check passed, skipping cmd", pc.id)
			pc.lifecycle.Reach("success")
			return
		}
	}

	if err := runShell(pc, state, grace, pc.step.Cmd, env, parent.name, parent.toolchain, streamKind+":cmd", stream, log); err != nil {
		log.Error("alpha", "provision[%s]: cmd failed: %v", pc.id, err)
		pc.lifecycle.Reach("failure")
		if failfast && !pc.step.Detached {
			state.requestShutdown()
		}
		return
	}

	if strings.TrimSpace(pc.step.Verify) != "" {
		if err := runShell(pc, state, grace, pc.step.Verify, env, parent.name, parent.toolchain, streamKind+":verify", stream, log); err != nil {
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
func runShell(pc *provisionCtx, state *alphaState, grace time.Duration, snippet string, env []string, svcName, toolchainLang, label string, stream *safeEncoder, log *zlog.Logger) error {
	cmd := exec.Command("/bin/sh", "-c", snippet)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Provisions inherit their parent service's toolchain pin so a
	// `service "go" "x"` migration provision runs with the same Go
	// version the daemon runs with — no surprise version skew between
	// the runtime and its orchestration steps.
	if toolchainLang != "" {
		if err := applyToolchainEnv(cmd, toolchainLang, state, log); err != nil {
			return fmt.Errorf("toolchain: %w", err)
		}
	}

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
	// External cancel — SIGTERM the group (service plus any forked
	// children), grace, then SIGKILL the group.
	_ = killGroup(cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-cmdDone:
	case <-time.After(grace):
		_ = killGroup(cmd.Process.Pid, syscall.SIGKILL)
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

// (Old serviceCtx.supervise was merged into bringupAndSupervise so a
// single goroutine owns the entire scheduled→running→ready→stopped
// lifecycle. cmd.Wait has exactly one owner across the whole codebase,
// which keeps "exec: Wait was already called" from being expressible.)

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

func (s *alphaState) addToolchain(tc *toolchainCtx) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toolchains == nil {
		s.toolchains = make(map[string]*toolchainCtx)
	}
	s.toolchains[tc.lang] = tc
}

func (s *alphaState) setReadiness(name, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readiness == nil {
		s.readiness = make(map[string]string)
	}
	s.readiness[name] = state
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
		info.Toolchain = s.config.Toolchain
		info.SysEnv = s.config.SysEnv
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

	// Materialize every pinned toolchain BEFORE services try to spawn.
	// One goroutine per (lang, version): EnsureMise + EnsureTools +
	// MiseEnv, cached on tc.env, Reach("ready"). Service goroutines
	// pick this up via the implicit `toolchain.<lang>@ready` dep
	// injected into runtime.after below — and via applyToolchainEnv's
	// belt-and-braces wait for code paths that don't go through the
	// after loop (provisions, build cmd).
	//
	// Allocated even when toolchains are already cached on disk: the
	// bringupToolchain goroutine short-circuits to Reach("ready") in
	// microseconds when the install is a no-op, and other entities can
	// always resolve `toolchain.X@ready` as a barrier.
	for lang, tcCfg := range newConfig.Toolchain {
		if tcCfg == nil || tcCfg.Version == "" {
			continue
		}
		tc := newToolchainCtx(lang, tcCfg.Version, tcCfg.Tools, tcCfg.Env)
		state.addToolchain(tc)
		go bringupToolchain(tc, log)
	}

	// Pre-allocate every entity FIRST so cross-refs in `after` resolve
	// the moment we spawn goroutines. Each ctx exposes its barriers
	// immediately even though cmd hasn't been started yet — the bringup
	// goroutine assigns sc.cmd inside itself once it's past the deps.
	var toBringup []*alphasfile.Service
	for _, svc := range services {
		if running[svc.Name()] && !toStop[svc.Name()] {
			continue
		}
		toBringup = append(toBringup, svc)
	}
	serviceCtxs := map[string]*serviceCtx{}
	for _, svc := range toBringup {
		sc := newServiceCtx(svc.Name(), svc.Toolchain)
		state.addService(sc)
		serviceCtxs[svc.Name()] = sc
	}
	type provEntry struct {
		pc       *provisionCtx
		parent   *serviceCtx
		detached bool
	}
	var provs []provEntry
	for _, svc := range toBringup {
		if svc.Runtime == nil {
			continue
		}
		parent := serviceCtxs[svc.Name()]
		for _, step := range svc.Runtime.Provision {
			pc := newProvisionCtx("service."+svc.Toolchain+"."+svc.Name(), step)
			state.addProvision(pc)
			provs = append(provs, provEntry{pc: pc, parent: parent, detached: step.Detached})
		}
	}

	// Spawn everything. Each goroutine selects on its deps and blocks
	// only when it must; bringup parallelism is naturally bounded by
	// the dep graph the user authored.
	for _, svc := range toBringup {
		go bringupAndSupervise(svc, serviceCtxs[svc.Name()], state, cfg, stream, log, failfast)
	}
	for _, p := range provs {
		go runProvision(p.pc, state, p.parent, cfg.shutdownGrace, stream, log, failfast)
	}

	// Wait for every service to settle (ready ∪ failed). doneBar is the
	// composed "outcome decided" barrier — fires regardless of which
	// terminal state was reached.
	for _, sc := range serviceCtxs {
		select {
		case <-sc.doneBar.Wait():
		case <-state.shutdownCh:
			return
		}
	}
	for name, sc := range serviceCtxs {
		if !sc.lifecycle.Reached("ready") {
			if failfast {
				stream.Send(&protocol.Event{Kind: protocol.EventError, Error: fmt.Sprintf("failfast: %s failed during bringup", name)})
				state.requestShutdown()
				return
			}
		}
	}

	// Now wait for non-detached provisions. Detached ones keep running
	// in the background; alpha shutdown will reap them with the rest.
	for _, p := range provs {
		if p.detached {
			continue
		}
		select {
		case <-p.pc.doneBar.Wait():
		case <-state.shutdownCh:
			return
		}
	}
	for _, p := range provs {
		if p.detached {
			continue
		}
		if !p.pc.lifecycle.Reached("success") && failfast {
			stream.Send(&protocol.Event{Kind: protocol.EventError, Error: fmt.Sprintf("failfast: provision %s failed", p.pc.id)})
			state.requestShutdown()
			return
		}
	}

	stream.Send(&protocol.Event{Kind: protocol.EventDone})
	log.Info("alpha", "bringup complete")
}

// implicitRuntimeAfter returns the full list of barrier refs a service
// must wait on before its build/runtime start — the user-declared
// `runtime.after` prepended by the implicit toolchain dep when the
// service's language is pinned in Alphasfile.toolchain{}.
//
// The toolchain entity becomes a hard precondition because every spawn
// (build cmd, runtime cmd, provision shells) layers tc.env onto its
// cmd.Env via applyToolchainEnv — and that env can only be cached once
// `mise install` + `gem install bundler` etc. have completed.
func implicitRuntimeAfter(svc *alphasfile.Service, state *alphaState) []string {
	var deps []string
	state.mu.RLock()
	_, pinned := state.toolchains[svc.Toolchain]
	state.mu.RUnlock()
	if pinned {
		deps = append(deps, "toolchain."+svc.Toolchain+"@ready")
	}
	if svc.Runtime != nil {
		deps = append(deps, svc.Runtime.After...)
	}
	return deps
}

// bringupAndSupervise runs one service's full lifecycle on a single
// goroutine: wait for runtime.after deps → prepare/build/start → race
// probe vs cmd self-exit → reach ready or failed → supervise until
// external stop or self-exit → reach stopped or failed (terminal).
// sc.done closes on return so anyone selecting on it (other bringup
// goroutines, handleConfigure's outcome wait, shutdownAll) unblocks.
func bringupAndSupervise(svc *alphasfile.Service, sc *serviceCtx, state *alphaState, cfg bringupConfig, stream *safeEncoder, log *zlog.Logger, failfast bool) {
	defer close(sc.done)
	// Note: DO NOT removeService here. Entity entries (and their
	// closed barriers) must remain in state.services for the rest of
	// the configure so late waiters — other provisions chained via
	// after, or a slow handler reading status — can still resolve the
	// canonical ref. Cleanup happens at the next stopServices (this
	// service replaced) or at shutdownAll (whole alpha gone).

	name := svc.Name()

	// 1. Wait for runtime.after deps. With no deps the loop's a no-op
	//    and we proceed immediately — pre-allocated barriers + uniform
	//    select treatment, no special case.
	//
	// Implicit dep: if this service's toolchain is pinned (an entity
	// exists for it), prepend `toolchain.<lang>@ready` so build AND
	// runtime AND provisions are gated on the toolchain having
	// materialized. Done here (not in the resolver) because pinning is
	// alpha-state's concern — federation children may pin a toolchain
	// that the resolver doesn't see locally.
	after := implicitRuntimeAfter(svc, state)
	for _, ref := range after {
		bt, err := state.resolveBarrier(ref)
		if err != nil {
			log.Error("alpha", "service[%s]: unresolvable dep %s: %v", name, ref, err)
			sc.lifecycle.Reach("failed")
			sc.build.Reach("failure")
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: "dep: " + err.Error()})
			return
		}
		select {
		case <-bt.target.Wait():
		case <-bt.fail.Wait():
			log.Error("alpha", "service[%s]: dep %s reached terminal failure", name, ref)
			sc.lifecycle.Reach("failed")
			sc.build.Reach("failure")
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: "dep " + ref + " failed"})
			return
		case <-sc.stopCh:
			sc.lifecycle.Reach("failed")
			sc.build.Reach("failure")
			return
		case <-state.shutdownCh:
			sc.lifecycle.Reach("failed")
			sc.build.Reach("failure")
			return
		}
	}

	log.Info("alpha", "bringup service=%s toolchain=%s", name, svc.Toolchain)
	stream.Send(&protocol.Event{Kind: protocol.EventServiceStart, Service: name})

	state.mu.RLock()
	agent := state.agentMode
	state.mu.RUnlock()

	// 2. Prepare (worktree materialize + build). Serialize per-primary
	//    so parallel bringups of services sharing a repo don't race on
	//    `git clone --bare`. Around the prepare call, drive the build
	//    lifecycle: running → success/failure. Cross-service waiters on
	//    `service.X.build@success` block here.
	sc.build.Reach("running")
	primaryKey := primaryKeyOf(svc)
	var repoDir string
	if primaryKey != "" {
		mu := state.primaryLockFor(primaryKey)
		mu.Lock()
		d, err := prepare(svc, name, agent, state, stream, log)
		mu.Unlock()
		if err != nil {
			log.Error("alpha", "prepare %s: %v", name, err)
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
			sc.build.Reach("failure")
			sc.lifecycle.Reach("failed")
			return
		}
		repoDir = d
	} else {
		d, err := prepare(svc, name, agent, state, stream, log)
		if err != nil {
			log.Error("alpha", "prepare %s: %v", name, err)
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
			sc.build.Reach("failure")
			sc.lifecycle.Reach("failed")
			return
		}
		repoDir = d
	}
	sc.build.Reach("success")
	bringupAndSuperviseStart(svc, sc, state, cfg, stream, log, failfast, repoDir, agent)
}

// primaryKeyOf returns the cross-service-shareable key for a primary
// (so monorepo services lock on the same string). Empty for services
// without a worktree-able primary (crate / install).
func primaryKeyOf(svc *alphasfile.Service) string {
	if svc.Package == nil {
		return ""
	}
	switch {
	case svc.Package.Git != "":
		return "git:" + svc.Package.Git
	case svc.Package.Src != "":
		return "src:" + svc.Package.Src
	}
	return ""
}

// bringupAndSuperviseStart is the half of bringupAndSupervise after
// prepare has produced a repoDir — does buildCmd, attachStdio, Start,
// races probe vs self-exit, then supervises the running process until
// external stop or self-exit. Split out so the per-primary lock doesn't
// have to wrap this much code.
func bringupAndSuperviseStart(svc *alphasfile.Service, sc *serviceCtx, state *alphaState, cfg bringupConfig, stream *safeEncoder, log *zlog.Logger, failfast bool, repoDir string, agent bool) {
	name := svc.Name()

	// env precedence (low→high): process env → federation parents'
	// file-level dotenv (root-first) → this Alphasfile's file-level
	// dotenv → this service's dotenv → this service's env block.
	state.mu.RLock()
	envFiles := append([]string{}, state.parentDotenv...)
	var sysenv []string
	if state.config != nil {
		if state.config.Dotenv != "" {
			envFiles = append(envFiles, state.config.Dotenv)
		}
		sysenv = state.config.SysEnv
	}
	state.mu.RUnlock()
	if svc.Runtime != nil && svc.Runtime.Dotenv != "" {
		envFiles = append(envFiles, svc.Runtime.Dotenv)
	}

	cmd, err := buildCmd(svc, repoDir, envFiles, agent, sysenv)
	if err != nil {
		log.Error("alpha", "build cmd %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		sc.lifecycle.Reach("failed")
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Layer mise + toolchain.<lang>.env onto cmd.Env. No-op when the
	// service's toolchain isn't pinned at all (Alphasfile lacks a
	// `toolchain { ... }` block) — falls back to system tools.
	if err := applyToolchainEnv(cmd, svc.Toolchain, state, log); err != nil {
		log.Error("alpha", "toolchain %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: "toolchain: " + err.Error()})
		sc.lifecycle.Reach("failed")
		return
	}

	stdout, stderr, ttyCleanup, err := attachStdio(cmd, *svc.Runtime.Log.TTY)
	if err != nil {
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		sc.lifecycle.Reach("failed")
		return
	}
	if err := cmd.Start(); err != nil {
		ttyCleanup()
		log.Error("alpha", "start %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		sc.lifecycle.Reach("failed")
		return
	}
	ttyCleanup()
	sc.cmd = cmd
	state.setReadiness(name, "probing")
	sc.lifecycle.Reach("running")
	log.Info("alpha", "started %s pid=%d argv=%v", name, cmd.Process.Pid, cmd.Args)

	go streamLines(name, "stdout", stdout, stream, log)
	go streamLines(name, "stderr", stderr, stream, log)

	// Own cmd.Wait here. No separate supervise goroutine — bringup AND
	// runtime supervision live on the same stack.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	probeCtx, cancelProbe := context.WithCancel(context.Background())
	defer cancelProbe()
	readyCh := make(chan error, 1)
	go func() { readyCh <- waitReady(probeCtx, svc, cfg.stabilization, log) }()

	// Race probe vs self-exit.
	select {
	case err := <-readyCh:
		if err != nil {
			state.setReadiness(name, "failed")
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: "readiness: " + err.Error()})
			_ = killGroup(cmd.Process.Pid, syscall.SIGTERM)
			select {
			case sc.exitErr = <-waitErr:
			case <-time.After(cfg.shutdownGrace):
				_ = killGroup(cmd.Process.Pid, syscall.SIGKILL)
				sc.exitErr = <-waitErr
			}
			sc.lifecycle.Reach("failed")
			return
		}
		sc.lifecycle.Reach("ready")
		state.setReadiness(name, "ready")
		stream.Send(&protocol.Event{Kind: protocol.EventServiceReady, Service: name})
	case sc.exitErr = <-waitErr:
		cancelProbe()
		if sc.exitErr != nil {
			log.Error("alpha", "%s exited before ready: %v", name, sc.exitErr)
		}
		state.setReadiness(name, "failed")
		msg := "exited before ready"
		if sc.exitErr != nil {
			msg = sc.exitErr.Error()
		}
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: msg})
		sc.lifecycle.Reach("failed")
		return
	}

	// Post-ready: wait for self-exit or external stop.
	var selfExit bool
	select {
	case sc.exitErr = <-waitErr:
		selfExit = true
	case <-sc.stopCh:
	case <-state.shutdownCh:
	}
	if selfExit {
		if sc.exitErr != nil {
			log.Error("alpha", "%s exited: %v", name, sc.exitErr)
		} else {
			log.Info("alpha", "%s exited cleanly", name)
		}
		sc.lifecycle.Reach("stopped")
		if failfast {
			log.Info("alpha", "failfast: %s exited after ready, requesting alpha shutdown", name)
			state.requestShutdown()
		}
		return
	}
	// External stop.
	log.Info("alpha", "SIGTERM %s pgid=%d", name, cmd.Process.Pid)
	_ = killGroup(cmd.Process.Pid, syscall.SIGTERM)
	select {
	case sc.exitErr = <-waitErr:
		log.Info("alpha", "%s exited after SIGTERM", name)
	case <-time.After(cfg.shutdownGrace):
		log.Info("alpha", "SIGKILL %s pgid=%d (grace expired)", name, cmd.Process.Pid)
		_ = killGroup(cmd.Process.Pid, syscall.SIGKILL)
		sc.exitErr = <-waitErr
	}
	sc.lifecycle.Reach("stopped")
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

// serviceEnv builds the child process environment: the sysenv-filtered
// alpha process env as a base, then each dotenv file in order, then the
// explicit env map — later entries override earlier ones.
//
// `allow` is the closed-world whitelist of OS env var names (from
// Alphasfile.SysEnv, federation-accumulated). Default-deny: anything
// not in `allow` is dropped from the parent shell's exports. Set to nil
// for an empty whitelist (strictest); see filterEnviron.
func serviceEnv(envFiles []string, env map[string]string, allow []string) []string {
	merged := map[string]string{}
	put := func(kv string) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	for _, kv := range filterEnviron(os.Environ(), allow) {
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

// filterEnviron keeps only the KEY=VAL entries whose KEY is in the allow
// list. Order is preserved (callers' dedup happens later). Closed-world:
// nil/empty allow ⇒ empty output (the user explicitly chose to expose
// nothing of the host shell to spawned services).
//
// This is the single chokepoint where the user's interactive shell stops
// leaking into spawned services. Anything not whitelisted here — RUBYLIB,
// GEM_HOME, BUNDLE_*, PYTHONPATH, CARGO_HOME, mise shims — vanishes.
func filterEnviron(env, allow []string) []string {
	if len(allow) == 0 {
		return nil
	}
	keep := make(map[string]struct{}, len(allow))
	for _, k := range allow {
		keep[k] = struct{}{}
	}
	out := make([]string, 0, len(allow))
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, ok := keep[k]; !ok {
			continue
		}
		out = append(out, kv)
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

func buildCmd(svc *alphasfile.Service, checkout string, envFiles []string, agent bool, sysenv []string) (*exec.Cmd, error) {
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
	// ALWAYS set cmd.Env (even when no dotenv / no overlay): with cmd.Env
	// nil, exec inherits the full alpha process env — defeating the
	// closed-world whitelist. serviceEnv with empty sysenv returns no
	// host vars, which is the desired strictest default.
	cmd.Env = serviceEnv(envFiles, runEnv, sysenv)
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
func prepare(svc *alphasfile.Service, name string, agent bool, state *alphaState, stream *safeEncoder, log *zlog.Logger) (string, error) {
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
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var be map[string]string
		if svc.Runtime != nil {
			be = phaseEnv(svc, svc.Runtime.BuildEnv, agent)
		}
		state.mu.RLock()
		var sysenv []string
		if state.config != nil {
			sysenv = state.config.SysEnv
		}
		state.mu.RUnlock()
		// ALWAYS set Env (even empty) — nil would inherit alpha's full env
		// and undo the closed-world whitelist.
		c.Env = serviceEnv(nil, be, sysenv)
		// Layer mise + toolchain.<lang>.env onto the build cmd too, so
		// `bundle install` / `cargo build` / `go build` see the pinned
		// interpreter — not whatever the user shell happens to expose.
		// Without this, ruby builds load bundler from user's mise install
		// and crash when stdlib loads come from System Ruby instead.
		if err := applyToolchainEnv(c, svc.Toolchain, state, log); err != nil {
			return "", fmt.Errorf("build toolchain: %w", err)
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
