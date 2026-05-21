package main

import (
	"bufio"
	"context"
	"encoding/json"
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
	"github.com/piotrkowalczuk/zordon/internal/parentwatch"
	"github.com/piotrkowalczuk/zordon/internal/probe"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/registry"
	"github.com/piotrkowalczuk/zordon/internal/source"
	"github.com/piotrkowalczuk/zordon/internal/zordonhome"
	"github.com/piotrkowalczuk/zordon/internal/tools"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
	"github.com/piotrkowalczuk/zordon/internal/zos"
)

// toolchainEnv blocks until the materialized toolchain entity for the
// given language reaches `ready` (or terminal failure), then returns
// its initial env: mise's `env --json` output, with tools.PostMiseEnv
// already merged in at install time, and the user's `toolchain.<lang>.env`
// overlay applied on top.
//
// This is the BASE the service's cmd.Env is built on — sysenv and the
// user's dotenv / service.env / phase env layer over it. The toolchain
// is the "initial environment", not the final overlay: per-service
// config from the user wins over toolchain defaults.
//
// Returns nil when the Alphasfile didn't pin this language — caller
// falls back to whatever's on PATH from sysenv.
//
// The install itself (EnsureMise / EnsureTools / mise install runtime
// version) happens ONCE per (lang, version) on a dedicated toolchainCtx
// goroutine launched at configure time — see bringupToolchain. The
// barrier wait here is belt-and-braces: in normal flow the implicit
// `toolchain.<lang>@ready` runtime.after dep has already gated the
// caller; this catches paths (provisions, build cmd) that might not
// have been gated explicitly.
func toolchainEnv(toolchainLang string, state *alphaState, log *zlog.Logger) (map[string]string, error) {
	_ = log // reserved for future "waited for ready" debug logs
	state.mu.RLock()
	tc := state.toolchains[toolchainLang]
	state.mu.RUnlock()
	if tc == nil {
		return nil, nil
	}
	select {
	case <-tc.lifecycle.Barrier("ready").Wait():
	case <-tc.lifecycle.Barrier(alphasfile.ToolchainTerminalFailure).Wait():
		return nil, fmt.Errorf("toolchain %s@%s: install failed (see earlier `toolchain` log lines)", tc.lang, tc.version)
	}
	// toolchain.<lang>.env (userEnv) overrides mise's defaults — both
	// are "toolchain initial envs", but the explicit `env { ... }`
	// block inside the user's toolchain block is the more specific
	// statement of intent.
	return tc.env.Join(tc.userEnv), nil
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


// fsHashFromSocket extracts the fs_hash segment from an alpha socket
// path of the form `<tmpdir>/zordon-<fsHash>/alpha.sock`. Empty
// string if the path doesn't match — registry calls then skip,
// avoiding "all entries" misfires under unexpected layout.
func fsHashFromSocket(sockPath string) string {
	// Parent dir name is "zordon-<fsHash>".
	dir := filepath.Base(filepath.Dir(sockPath))
	const prefix = "zordon-"
	if !strings.HasPrefix(dir, prefix) {
		return ""
	}
	return strings.TrimPrefix(dir, prefix)
}

// relookupPath re-resolves cmd.Path against cmd.Env's PATH instead
// of whatever os.Environ()'s PATH was when exec.Command first called
// LookPath. Needed when:
//
//  1. exec.Command(name, ...) was constructed with a bare program
//     name. Go's LookPath ran immediately using os.Environ(), so
//     cmd.Path was locked to the FIRST PATH's result.
//  2. Later we overlay a different env (mise toolchain, dotenv, ...)
//     onto cmd.Env. PATH in cmd.Env now points at a different
//     install, but cmd.Path still references the original lookup.
//
// Without this, an explicit `build { cmd = ["go", "build", ...] }`
// would always invoke the system `go` even when toolchain.go is
// pinned to a different version via mise.
//
// No-op when:
//   - cmd.Path is already absolute or contains a path separator
//     (user passed an explicit path; honor it as-is).
//   - cmd.Env's PATH is empty (no overlay to apply).
//   - the lookup against the overlay PATH finds nothing (caller
//     gets a clean error from exec when it eventually runs).
func relookupPath(cmd *exec.Cmd) error {
	if cmd == nil || len(cmd.Args) == 0 {
		return nil
	}
	// Decide based on the ORIGINAL bare name in Args[0], not cmd.Path —
	// exec.Command's LookPath always writes an absolute Path even when
	// the caller passed a bare name, so cmd.Path being absolute is
	// uninformative. If Args[0] already has a separator the user
	// passed an explicit path; honor it.
	name := cmd.Args[0]
	if strings.ContainsAny(name, "/"+string(filepath.Separator)) {
		return nil
	}
	pathVar := ""
	for _, kv := range cmd.Env {
		if rest, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVar = rest
		}
	}
	if pathVar == "" {
		return nil
	}
	for _, dir := range filepath.SplitList(pathVar) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			cmd.Path = candidate
			// exec.Command cached a LookPath error against alpha's PATH;
			// Start() returns it even after we fix Path. Clear it now that
			// we've resolved against the overlay PATH.
			cmd.Err = nil
			return nil
		}
	}
	return fmt.Errorf("relookupPath: %q not found on cmd.Env PATH", name)
}

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

// closeParentW closes the tommy parent-death pipe write end alpha holds
// for this service, once. Idempotent and nil-safe so every service exit
// path can call it without bookkeeping. Closing it is fd hygiene, not
// the death signal — the signal is alpha's own death closing it for us.
func closeParentW(sc *serviceCtx) {
	if sc != nil && sc.parentW != nil {
		_ = sc.parentW.Close()
		sc.parentW = nil
	}
}

// resolveTommyBin locates the tommy wrapper alpha interposes in front of
// every service. tommy is security-relevant — it is exec'd ahead of
// EVERY service — so resolution is deliberately NOT $PATH-based: a
// writable dir earlier on $PATH could otherwise substitute a malicious
// "tommy" into every spawn. Only two trusted sources, in order:
//
//  1. $ZORDON_TOMMY_BIN — an explicit operator/test override. Setting
//     alpha's environment is the same trust level as launching alpha,
//     so this is NOT an attacker-reachable redirection (it is not a
//     directory swap like $PATH); it exists so tests can pin a freshly
//     built binary and operators can relocate it deliberately.
//  2. a "tommy" sibling of alpha's OWN executable — the secure default.
//     That directory can't be substituted without write access next to
//     the alpha binary itself, and anyone with that can already replace
//     alpha, so nothing is gained. The make build (bin/alpha +
//     bin/tommy) and `go install ./cmd/...` (shared GOBIN) both satisfy
//     it with zero configuration.
//
// No $PATH fallback by design: tommy is exec'd ahead of EVERY service,
// so a writable dir earlier on $PATH must never be able to substitute a
// malicious "tommy" into every spawn. Returns "" when neither resolves;
// the caller logs loudly and falls back to spawning the service
// unwrapped (degraded — orphan guarantee lost — but bringup proceeds).
func resolveTommyBin() string {
	if v := os.Getenv("ZORDON_TOMMY_BIN"); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "tommy")
		if info, err := os.Stat(cand); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return cand
		}
	}
	return ""
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
	zordonHome := runFlags.StringLong("zordon-home", "", "directory holding shared mise install and toolchain cache (overrides $ZORDON_HOME; defaults to ~/.zordon)")
	runCmd := &ff.Command{
		Name:      "run",
		Usage:     "alpha run --socket <path> [--log-file <path>]",
		ShortHelp: "run the alpha supervisor; signals readiness on $ZORDON_READY_FD if set",
		Flags:     runFlags,
		Exec: func(ctx context.Context, args []string) error {
			return runAlpha(ctx, *sockPath, *logFile, zordonhome.Resolve(*zordonHome), *stabilization, *shutdownGrace)
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
	zordonHome     string   // ~/.zordon — host-wide registry root (empty ⇒ registry disabled)
	tommyBin       string   // path to the tommy wrapper interposed before every service (empty ⇒ spawn unwrapped)
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
	//
	// shutdownReason carries the human-readable cause of the close
	// (e.g. "failfast: api exited before ready", "SIGTERM received"). It
	// is set INSIDE shutdownOnce, BEFORE the close, so any goroutine that
	// observes the channel close sees the reason via channel-close
	// release semantics — no mutex needed for the read.
	shutdownCh     chan struct{}
	shutdownOnce   sync.Once
	shutdownReason string

	// parentW watches the pipe zordon holds open for us; closed when
	// zordon dies (kernel-closed write end ⇒ EOF on our read end).
	// Stopped after the first configure round-trip finishes, because
	// that is the point at which zordon is allowed to detach cleanly:
	// EventDone has been emitted, zordon will exit (closing the pipe)
	// without that close meaning death. Nil when alpha was launched
	// outside zordon (no ZORDON_PARENT_FD).
	parentW *parentwatch.Watcher

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

// requestShutdown closes shutdownCh once and records why. The reason is
// surfaced in the SIGTERM log line of every supervisor that picks up
// the shutdown — so an operator skimming alpha.log can immediately
// tell whether the shutdown was a signal, an OpShutdown control op, or
// a specific failfast trigger.
func (s *alphaState) requestShutdown(reason string) {
	s.shutdownOnce.Do(func() {
		s.shutdownReason = reason
		close(s.shutdownCh)
	})
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

// toolchainCtx is one materialized language pin (lang + version) —
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
	env zos.EnvironmentVariables
	// userEnv is the Alphasfile-declared overlay (`toolchain { <lang>
	// { env = {...} } }`). Set at allocation; never mutated.
	userEnv zos.EnvironmentVariables
	// tools is the name→version map of language-native tools to
	// install into this pinned interpreter (bundler@2.5.3, dlv@..., etc).
	tools     map[string]string
	lifecycle *lifecycle.Instance
	doneBar   *barrier.Barrier // Any(ready, failed)
}

func newToolchainCtx(lang, version string, toolsMap map[string]string, userEnv zos.EnvironmentVariables) *toolchainCtx {
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
func bringupToolchain(tc *toolchainCtx, zordonHome string, log *zlog.Logger) {
	tc.lifecycle.Reach("installing")
	log.Info("alpha", "toolchain %s@%s: installing", tc.lang, tc.version)

	if zordonHome == "" {
		log.Error("alpha", "toolchain %s: zordon home not configured", tc.lang)
		tc.lifecycle.Reach("failed")
		return
	}
	bin, err := tools.EnsureMise(zordonHome, logWriter(log, "mise-install"))
	if err != nil {
		log.Error("alpha", "toolchain %s: ensure mise: %v", tc.lang, err)
		tc.lifecycle.Reach("failed")
		return
	}
	// ResolveDataDir uses zordonHome as the walk-up anchor; the
	// default toolchain root lives directly under it. Alpha doesn't
	// have a per-service cwd to pivot from at this stage (we
	// materialize toolchains BEFORE services run).
	defaultDataDir := filepath.Join(zordonHome, "toolchain")
	dataDir := tools.ResolveDataDir(zordonHome, defaultDataDir, tc.lang, tc.version)
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
	tools.PostMiseEnv(tc.lang, env)

	// nodejs only: refresh Corepack (bundled corepack in older node
	// distributions has a stale keyring → pnpm install fails). After
	// this, env carries shim-dir + refreshed-corepack-bin on PATH, so
	// pnpm/yarn invocations through `<pm> install` find the managed
	// shims rather than node's own bundled corepack.
	if tc.lang == alphasfile.ToolchainNode {
		if err := tools.EnsureNodeCorepack(bin, dataDir, tc.version, env, logWriter(log, "corepack")); err != nil {
			log.Error("alpha", "toolchain %s: corepack: %v", tc.lang, err)
			tc.lifecycle.Reach("failed")
			return
		}
	}

	tc.env = env
	tc.lifecycle.Reach("ready")
	log.Info("alpha", "toolchain %s@%s: ready", tc.lang, tc.version)
}

// Env precedence (lowest to highest), assembled in serviceEnv:
//
//  1. sysenv whitelist filters host env → base.
//  2. mise's `env --json` → toolchain reality (PATH, GOROOT, GEM_PATH, ...).
//  3. tools.PostMiseEnv → per-language pin reinforcement (e.g. Go's
//     GOTOOLCHAIN=local). Lives in internal/tools/lang.go so alpha
//     stays language-agnostic. Merged into tc.env at toolchain bringup.
//  4. toolchain.<lang>.env → user-explicit overrides at the language
//     level. Layers 2–4 together form the "toolchain initial envs"
//     returned by toolchainEnv.
//  5. dotenv files (federation parents → this Alphasfile → service).
//  6. service.env + phase env (build/runtime) + agent overlay →
//     per-service overrides at the top.

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
	// parentW is the write end of the tommy parent-death pipe. alpha
	// holds it open for the whole service lifetime and never writes to
	// it: tommy blocks reading the matching read end, so the kernel
	// closing this fd when alpha dies (SIGKILL/OOM included) is what
	// tells tommy to reap the service. Closed explicitly on clean exit
	// so the fd doesn't leak across restarts. nil when tommy was
	// unavailable and the service runs unwrapped.
	parentW *os.File
	// stopCh / stopReason — see requestStop. stopReason is set inside
	// stopOnce BEFORE close, so any goroutine that observes the
	// channel close also sees the reason (channel-close release sync).
	stopCh     chan struct{}
	stopOnce   sync.Once
	stopReason string
	done       chan struct{}

	lifecycle *lifecycle.Instance
	// doneBar is the synthetic "outcome decided" barrier (Any(ready,
	// failed)). It lives outside the lifecycle DAG because no single set
	// of predecessors can model "either of these happened".
	doneBar *barrier.Barrier

	// build is the parallel one-shot lifecycle for the service's build
	// phase (the `build { cmd = [...] }` step, or the toolchain default
	// when absent). Exposed as `service.<tc>.<n>.build@<state>` so other
	// services sharing the source checkout can wait on its success
	// before their own runtime starts (vendor dir populated, generated
	// code present, etc.).
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

// requestStop closes this service's stopCh once and records why.
// Reasons surface in the SIGTERM log line emitted by the supervisor —
// e.g. "alpha shutdown: <shutdownReason>" (whole-alpha teardown) or
// "reconfigure: replaced by new manifest" (this service's spec changed
// and a fresh ctx is taking over).
func (sc *serviceCtx) requestStop(reason string) {
	sc.stopOnce.Do(func() {
		sc.stopReason = reason
		close(sc.stopCh)
	})
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

	stopCh     chan struct{}
	stopOnce   sync.Once
	stopReason string
	done       chan struct{}

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

// requestStop closes this provision's stopCh once and records why.
// Same shape and reasoning as serviceCtx.requestStop.
func (pc *provisionCtx) requestStop(reason string) {
	pc.stopOnce.Do(func() {
		pc.stopReason = reason
		close(pc.stopCh)
	})
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
// user is responsible for declaring `after = [self.runtime.ready, ...]`
// if the snippet needs the service operational; `after = never` makes
// it latent — not auto-run here at all), runs check (skip cmd if
// check=0), runs cmd, runs verify (best effort). With a CmdRef the
// check/cmd/verify snippets come from the referenced template instead.
// Reach success/failure terminal, close done. Failure under failfast on
// a non-detached provision triggers global shutdown.
func runProvision(pc *provisionCtx, state *alphaState, parent *serviceCtx, grace time.Duration, stream *safeEncoder, log *zlog.Logger, failfast bool) {
	defer close(pc.done)
	// Same reasoning as bringupAndSupervise: keep the entry in
	// state.provisions so other entities that wait on this provision's
	// success/failure can still resolve it after the goroutine has
	// returned. Map cleanup is the next configure's or shutdown's job.

	pc.lifecycle.Reach("scheduled")

	// Snippets to run. By default the provision's own; with a CmdRef
	// they come from the referenced template provision — resolved at
	// the *provider's* eval time — while env, cwd and the barrier stay
	// this (invoking) provision's. That's what lets one service expose
	// an action many peers run for themselves. Any provision can be a
	// template: `after = never` is not required, it only means the
	// template doesn't ALSO auto-run for its own service.
	checkSnippet, cmdSnippet, verifySnippet := pc.step.Check, pc.step.Cmd, pc.step.Verify
	if pc.step.CmdRef != "" {
		state.mu.RLock()
		tpl := state.provisions[pc.step.CmdRef]
		state.mu.RUnlock()
		if tpl == nil {
			log.Error("alpha", "provision[%s]: cmd references unknown provision %q", pc.id, pc.step.CmdRef)
			pc.lifecycle.Reach("failure")
			if failfast && !pc.step.Detached {
				state.requestShutdown(fmt.Sprintf("failfast: provision %s references unknown provision %q", pc.id, pc.step.CmdRef))
			}
			return
		}
		checkSnippet, cmdSnippet, verifySnippet = tpl.step.Check, tpl.step.Cmd, tpl.step.Verify
	}

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
	//   toolchain env (mise + PostMiseEnv + toolchain.<lang>.env)
	//   parent service's env { } block
	//   parent service's runtime.env (if any) — keeps migrate scripts in
	//                                           sync with the daemon's vars
	//   parent service's agent.env when --agent
	//   provision step's own env { }
	//
	// Toolchain envs sit BELOW parent service.env / pc.step.Env so the
	// user's per-service overrides take precedence — same model as the
	// runtime spawn. Inheriting the parent service's env is what
	// `db_url = ...` in service.env needs to be visible to a `migrate`
	// provision without the user having to retype it.
	state.mu.RLock()
	var sysenv []string
	var parentSvc *alphasfile.Service
	agentMode := state.agentMode
	alphasfileDir := ""
	if state.alphasfilePath != "" {
		alphasfileDir = filepath.Dir(state.alphasfilePath)
	}
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
	tcEnv, err := toolchainEnv(parent.toolchain, state, log)
	if err != nil {
		log.Error("alpha", "provision[%s]: toolchain: %v", pc.id, err)
		pc.lifecycle.Reach("failure")
		if failfast && !pc.step.Detached {
			state.requestShutdown(fmt.Sprintf("failfast: provision %s toolchain env: %v", pc.id, err))
		}
		return
	}
	var parentEnv map[string]string
	if parentSvc != nil {
		parentEnv = phaseEnv(parentSvc, parentSvc.Runtime.RunEnv, agentMode)
	}
	env := zos.NewEnvironmentVariablesFromHost(sysenv).Join(tcEnv, parentEnv, pc.step.Env).Slice()
	cwd := provisionCwd(parentSvc, alphasfileDir)

	// Stream labels are just the phase — the canonical entity ID is
	// already part of the log line's service column and the provision
	// name is in the messages alpha emits. A second
	// "provision:<name>:<phase>" wrapping was just visual noise.
	if strings.TrimSpace(checkSnippet) != "" {
		if err := runShell(pc, state, grace, checkSnippet, env, cwd, parent.name, "check", stream, log); err == nil {
			log.Info("alpha", "provision[%s]: check passed, skipping cmd", pc.id)
			pc.lifecycle.Reach("success")
			return
		}
	}

	if err := runShell(pc, state, grace, cmdSnippet, env, cwd, parent.name, "cmd", stream, log); err != nil {
		log.Error("alpha", "provision[%s]: cmd failed: %v", pc.id, err)
		pc.lifecycle.Reach("failure")
		if failfast && !pc.step.Detached {
			state.requestShutdown(fmt.Sprintf("failfast: provision %s cmd failed: %v", pc.id, err))
		}
		return
	}

	if strings.TrimSpace(verifySnippet) != "" {
		if err := runShell(pc, state, grace, verifySnippet, env, cwd, parent.name, "verify", stream, log); err != nil {
			log.Error("alpha", "provision[%s]: verify failed (best-effort, not fatal): %v", pc.id, err)
		}
	}

	log.Info("alpha", "provision[%s]: success", pc.id)
	pc.lifecycle.Reach("success")
}

// provisionCwd is where a provision shell runs. Preference: the parent
// service's source dir, so language-native tooling (bundle exec, go
// run ./cmd/..., etc.) resolves manifests and package paths the same
// way a developer running them by hand would. Fallback for use-only
// services with no source checkout: the Alphasfile's own directory —
// the project root a user invoking `zordon start` would expect, so a
// relative provision cmd like `./scripts/foo.sh` resolves against it.
//
// Pulled out so a unit test can pin the rule without spinning real
// shells: see TestProvisionCwd in barriers_test.go.
func provisionCwd(parentSvc *alphasfile.Service, alphasfileDir string) string {
	if parentSvc != nil && parentSvc.Runtime != nil && parentSvc.Runtime.Dir != "" {
		return parentSvc.Runtime.Dir
	}
	return alphasfileDir
}

// runShell executes a single shell snippet (check / cmd / verify) with
// the provision's env (already toolchain-layered by the caller),
// streams its stdout+stderr through the safe encoder, and reacts to
// pc.stopCh / state.shutdownCh by SIGTERM → grace → SIGKILL. Returns
// the cmd.Wait() error (nil on exit 0).
func runShell(pc *provisionCtx, state *alphaState, grace time.Duration, snippet string, env []string, cwd, svcName, label string, stream *safeEncoder, log *zlog.Logger) error {
	cmd := exec.Command("/bin/sh", "-c", snippet)
	cmd.Env = env
	cmd.Dir = cwd // provisions run from the parent service's source dir
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

	var stopReason string
	select {
	case <-cmdDone:
		streamWg.Wait()
		return cmdErr
	case <-pc.stopCh:
		stopReason = pc.stopReason
	case <-state.shutdownCh:
		stopReason = state.shutdownReason
	}
	// External cancel — SIGTERM the group (service plus any forked
	// children), grace, then SIGKILL the group.
	log.Info("alpha", "SIGTERM provision[%s] %s pgid=%d (reason: %s)", pc.id, label, cmd.Process.Pid, stopReason)
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

	// Forward the alpha-wide reason verbatim — supervisors log it as
	// the SIGTERM cause. Empty when shutdownCh fires before any
	// requestShutdown() (shouldn't happen in normal flow, but the
	// fallback string keeps the log readable).
	reason := s.shutdownReason
	if reason == "" {
		reason = "alpha shutdown"
	}
	for _, sc := range services {
		sc.requestStop(reason)
	}
	for _, pc := range provisions {
		pc.requestStop(reason)
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
		sc.requestStop("reconfigure: service replaced by new manifest")
	}
	for _, pc := range pickedProvs {
		pc.requestStop("reconfigure: parent service replaced by new manifest")
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
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
		tmp, err := os.CreateTemp(dir, ".zordon-"+filepath.Base(f.Path)+".*")
		if err != nil {
			return fmt.Errorf("temp file in %s: %w", dir, err)
		}
		if _, err := tmp.WriteString(f.Body); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmp.Name())
			return fmt.Errorf("close %s: %w", tmp.Name(), err)
		}
		if err := os.Rename(tmp.Name(), f.Path); err != nil {
			_ = os.Remove(tmp.Name())
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

func runAlpha(_ context.Context, sockPath, logPath, zordonHome string, stabilization, shutdownGrace time.Duration) error {
	if sockPath == "" {
		return errors.New("--socket is required")
	}

	// Drain SIGPIPE before anything else writes to stderr. Once zordon
	// (our spawner) dies, the stderr it inherited to us has no reader;
	// Go's default is to kill us on EPIPE — which would happen BEFORE
	// the parent-death watcher gets a chance to drive a graceful
	// shutdown. parentwatch.IgnoreSIGPIPE turns those writes into
	// harmless EPIPE errors.
	parentwatch.IgnoreSIGPIPE()

	// Arm the parent-death watch BEFORE we open any file: an early
	// OpenFile could otherwise be assigned the very fd zordon passed
	// to us, and parentwatch would then attach to the wrong file.
	// Watch claims the fd via os.NewFile (and marks it close-on-exec)
	// right here, so any subsequent open gets a higher number.
	//
	// The reaction goroutine cannot run yet — alphaState isn't built —
	// so we just capture the watcher and wire it in below.
	parentW, err := parentwatch.Watch("ZORDON_PARENT_FD",
		parentwatch.WithErrorLog(func(err error) {
			fmt.Fprintf(os.Stderr, "alpha: parentwatch: %v\n", err)
		}))
	if err != nil {
		return fmt.Errorf("parentwatch: %w", err)
	}

	logF, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logF.Close()

	log := zlog.New(logF, false)
	log.Info("alpha", "starting pid=%d socket=%s", os.Getpid(), sockPath)

	// Reap orphans from a previous alpha for THIS fs_hash before
	// binding the control socket: a service binary from the prior
	// run holding a TCP port would make a fresh bind fail otherwise.
	// fsHash is derived from the socket path (sockPath looks like
	// $TMPDIR/zordon-<fsHash>/alpha.sock — the segment between
	// "zordon-" and the next path separator is the identifier).
	fsHash := fsHashFromSocket(sockPath)
	if zordonHome != "" && fsHash != "" {
		if err := registry.ReapByFsHash(zordonHome, fsHash, shutdownGrace, log); err != nil {
			log.Error("alpha", "registry reap: %v", err)
		}
	}

	ln, err := control.Listen(sockPath)
	if err != nil {
		return fmt.Errorf("control listen: %w", err)
	}
	defer ln.Close()
	log.Info("alpha", "listening on %s", sockPath)

	tommyBin := resolveTommyBin()
	if tommyBin == "" {
		log.Error("alpha", "tommy wrapper not found ($ZORDON_TOMMY_BIN / alpha sibling / $PATH) — services run UNWRAPPED; an alpha SIGKILL/OOM will orphan them")
	} else {
		log.Info("alpha", "tommy wrapper: %s", tommyBin)
	}

	state := &alphaState{
		startedAt:  time.Now(),
		fsHash:     fsHash,
		zordonHome: zordonHome,
		tommyBin:   tommyBin,
		shutdownCh: make(chan struct{}),
	}
	cfg := bringupConfig{stabilization: stabilization, shutdownGrace: shutdownGrace}

	// Park the watcher on state so handleConfigure can drop it after
	// the configure round-trip completes. We deliberately KEEP the
	// watch armed across the listener-up + bringup window: signalReady
	// only means "alpha is listening", not "zordon may detach". zordon
	// blocks in pushConfigure until EventDone (or EventError), so if
	// it dies anywhere in that window we want graceful shutdown here.
	state.parentW = parentW

	// Wire parent-death into the shutdown channel. Two cases:
	//   - parentW is nil ⇒ no ZORDON_PARENT_FD in env (alpha launched
	//     standalone). Died() returns nil; the goroutine only exits via
	//     shutdownCh.
	//   - parentW.Died() fires before Stop is called ⇒ zordon died
	//     during bringup; trigger the same shutdown path as a SIGTERM
	//     or control-socket OpShutdown.
	go func() {
		select {
		case <-parentW.Died():
			log.Error("alpha", "zordon parent died during bringup; shutting down")
			state.requestShutdown("parent (zordon) died during bringup")
		case <-state.shutdownCh:
		}
	}()

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
		state.requestShutdown(fmt.Sprintf("signal: %s received by alpha", sig))
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
		state.requestShutdown("control socket: shutdown op received")
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
	// One configure round-trip is the contract with zordon: once we
	// have emitted EventDone (or EventError), zordon will detach and
	// exit, closing its end of the parent-watch pipe. Dropping the
	// watch here turns that kernel-closed write end from "false-
	// positive death" into a harmless no-op.
	defer state.parentW.Stop()

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
		go bringupToolchain(tc, state.zordonHome, log)
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
			// Latent provisions (`after = never`) are registered so they
			// resolve as barriers and can be used as CmdRef templates by
			// other services, but no goroutine runs them at bringup —
			// they fire only when a peer invokes them for itself.
			state.addProvision(pc)
			if step.Latent {
				continue
			}
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
				reason := fmt.Sprintf("failfast: %s failed during bringup", name)
				stream.Send(&protocol.Event{Kind: protocol.EventError, Error: reason})
				state.requestShutdown(reason)
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
			reason := fmt.Sprintf("failfast: provision %s failed", p.pc.id)
			stream.Send(&protocol.Event{Kind: protocol.EventError, Error: reason})
			state.requestShutdown(reason)
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
			if failfast {
				state.requestShutdown(fmt.Sprintf("failfast: service %s unresolvable dep %s: %v", name, ref, err))
			}
			return
		}
		select {
		case <-bt.target.Wait():
		case <-bt.fail.Wait():
			// Dep already failed. The first failure in the bringup chain
			// is the one that triggers shutdown; subsequent dep-failure
			// returns are just the chain unwinding, so don't double-fire
			// requestShutdown here (it's idempotent, but the log noise
			// would mislead).
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

	// 2. Prepare. Two stages with different concurrency profiles:
	//
	//    a. Worktree materialization (git clone --bare + git worktree
	//       add): serialized per-primary via state.primaryLockFor so
	//       monorepo siblings don't race on the same bare clone path.
	//       Cheap when the bare already exists — just a `git fetch`.
	//    b. Build: parallel across all services. Each writes to its own
	//       dest dir / bin dir, so there's nothing to serialize. The
	//       lock is RELEASED before this step kicks off, which means a
	//       monorepo with five services builds five in parallel rather
	//       than queueing behind whichever sibling got the lock first.
	//
	//    Around both stages, drive the build lifecycle:
	//    running → success/failure. Cross-service waiters on
	//    `service.X.build@success` block here.
	sc.build.Reach("running")
	prepareFail := func(err error) {
		log.Error("alpha", "prepare %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		sc.build.Reach("failure")
		sc.lifecycle.Reach("failed")
		// Failfast: a failed prepare means this service will never reach
		// ready. Trigger the alpha-wide shutdown immediately so peers
		// still in their own dep-wait or post-prepare gate bail out
		// instead of marching on to runtime spawn. Without this, peers
		// would only notice at the outer bringup loop's final reckoning
		// (after every doneBar has fired) — i.e., far too late.
		if failfast {
			state.requestShutdown(fmt.Sprintf("failfast: service %s prepare failed: %v", name, err))
		}
	}
	var (
		repoDir string
		err     error
	)
	if primaryKey := primaryKeyOf(svc); primaryKey != "" {
		mu := state.primaryLockFor(primaryKey)
		mu.Lock()
		repoDir, err = prepareWorktree(svc, name, stream, log)
		mu.Unlock()
	} else {
		repoDir, err = prepareWorktree(svc, name, stream, log)
	}
	if err != nil {
		prepareFail(err)
		return
	}
	if err := prepareBuild(svc, name, repoDir, agent, state, stream, log); err != nil {
		prepareFail(err)
		return
	}
	sc.build.Reach("success")

	// Shutdown gate: if failfast / signal / external stop fired during
	// prepare (which can take minutes for cargo install / go build /
	// bundle install), don't burn cycles starting a runtime that will
	// be SIGTERM'd a second later. Previously a service that finished
	// its build right after another's failfast would still spawn the
	// daemon, hit ready, then be killed — wasted compute + noisy logs.
	select {
	case <-sc.stopCh:
		sc.lifecycle.Reach("failed")
		return
	case <-state.shutdownCh:
		sc.lifecycle.Reach("failed")
		return
	default:
	}

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
		envFiles = append(envFiles, state.config.Dotenv...)
		sysenv = state.config.SysEnv
	}
	state.mu.RUnlock()
	if svc.Runtime != nil {
		envFiles = append(envFiles, svc.Runtime.Dotenv...)
	}

	// Resolve toolchain envs FIRST — they're the initial environment
	// (PATH/GOROOT/GEM_PATH/...) the service runs under. Dotenv +
	// service.env / phase env layer over them inside buildCmd. Blocks
	// until the toolchain entity reaches ready (no-op when none).
	tcEnv, err := toolchainEnv(svc.Toolchain, state, log)
	if err != nil {
		log.Error("alpha", "toolchain %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: "toolchain: " + err.Error()})
		sc.lifecycle.Reach("failed")
		return
	}

	cmd, err := buildCmd(svc, repoDir, envFiles, agent, sysenv, tcEnv)
	if err != nil {
		log.Error("alpha", "build cmd %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		sc.lifecycle.Reach("failed")
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// exec.Command resolved cmd.Path against alpha's os.Environ() PATH
	// when the command was constructed. cmd.Env's PATH now reflects the
	// toolchain pin (mise's GOROOT/bin) which may differ from system —
	// re-resolve so a bare `go` / `cargo` etc. invocation lands on the
	// pinned binary, not whatever alpha's PATH had.
	if err := relookupPath(cmd); err != nil {
		log.Error("alpha", "relookup %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: "relookup: " + err.Error()})
		sc.lifecycle.Reach("failed")
		return
	}

	// Interpose tommy in front of the resolved service binary. tommy
	// execs the real binary and, if alpha dies without a chance to
	// clean up (SIGKILL / OOM), reaps it so it never orphans. tommy
	// shares the alpha-assigned process group with the service (it does
	// NOT make a new one), so the registry row, killGroup, and status
	// pid below all stay correct against cmd.Process.Pid exactly as
	// before — tommy is transparent to alpha's supervision. When tommy
	// is unavailable we already logged at startup; run unwrapped (the
	// pre-tommy behavior) rather than fail bringup.
	if state.tommyBin != "" {
		pr, pw, perr := parentwatch.Pipe()
		if perr != nil {
			log.Error("alpha", "tommy pipe %s: %v", name, perr)
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: "tommy pipe: " + perr.Error()})
			sc.lifecycle.Reach("failed")
			return
		}
		wrapped := exec.Command(state.tommyBin, append([]string{
			"--shutdown-grace", cfg.shutdownGrace.String(),
			"--", cmd.Path,
		}, cmd.Args[1:]...)...)
		wrapped.Dir = cmd.Dir
		// tommy is env-transparent: it inherits this env and passes it
		// to the service verbatim. parentwatch.Attach sets up
		// ExtraFiles + TOMMY_PARENT_FD env in lock-step.
		wrapped.Env = append([]string(nil), cmd.Env...)
		parentwatch.Attach(wrapped, pr, "TOMMY_PARENT_FD")
		wrapped.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		sc.parentW = pw
		cmd = wrapped
		// alpha keeps only the write end (pw, on sc) open; tommy dups
		// its own read end at Start, so drop ours at function exit.
		defer pr.Close()
	}

	stdout, stderr, ttyCleanup, err := attachStdio(cmd, *svc.Runtime.Log.TTY)
	if err != nil {
		closeParentW(sc)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		sc.lifecycle.Reach("failed")
		return
	}
	if err := cmd.Start(); err != nil {
		ttyCleanup()
		closeParentW(sc)
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

	// Record the spawn in the host-wide registry so a future alpha
	// boot for this same fs_hash can reap us if we die uncleanly.
	// Best-effort: a registry write error doesn't fail the service —
	// it just means orphan cleanup won't catch THIS service later.
	registered := false
	if state.zordonHome != "" && state.fsHash != "" {
		port := 0
		if p := readinessOf(svc); p != nil && p.HTTP != nil {
			port = p.HTTP.Port
		}
		err := registry.Register(state.zordonHome, registry.Entry{
			Port:    port,
			FsHash:  state.fsHash,
			Service: name,
			PGID:    cmd.Process.Pid, // Setpgid=true ⇒ pid == pgid
		})
		if err != nil {
			log.Error("alpha", "registry register %s: %v", name, err)
		} else {
			registered = true
		}
	}
	// Drop the registry row + tommy pipe on EVERY return path below —
	// clean stop, external stop, self-exit, readiness fail, AND
	// exit-before-ready. The earlier shape only deferred this inside
	// the post-ready branch, so a service that died during bringup
	// leaked its registry row until the next alpha boot's reaper —
	// which is exactly what TestLifecycle_failureSummaryShowsCulprit
	// catches via AssertNoLeftovers.
	defer func() {
		if registered {
			if err := registry.Remove(state.zordonHome, state.fsHash, name); err != nil {
				log.Error("alpha", "registry remove %s: %v", name, err)
			}
		}
		// Release the tommy parent-death pipe. By this point the
		// service has exited and tommy with it, so this is just fd
		// hygiene across restarts — not the death signal.
		closeParentW(sc)
	}()

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
			log.Info("alpha", "SIGTERM %s pgid=%d (reason: readiness probe failed: %v)", name, cmd.Process.Pid, err)
			_ = killGroup(cmd.Process.Pid, syscall.SIGTERM)
			select {
			case sc.exitErr = <-waitErr:
			case <-time.After(cfg.shutdownGrace):
				_ = killGroup(cmd.Process.Pid, syscall.SIGKILL)
				sc.exitErr = <-waitErr
			}
			sc.lifecycle.Reach("failed")
			if failfast {
				state.requestShutdown(fmt.Sprintf("failfast: service %s readiness probe failed: %v", name, err))
			}
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
		if failfast {
			state.requestShutdown(fmt.Sprintf("failfast: service %s exited before ready: %s", name, msg))
		}
		return
	}

	// Post-ready: wait for self-exit or external stop. stopReason
	// captures WHICH external stop fired so the SIGTERM log line
	// below can tell the operator "alpha killed this because X" —
	// e.g. "control socket: shutdown op received" vs "failfast: db
	// exited before ready" vs "reconfigure: replaced by new manifest".
	// Reading the reason after the channel close is safe: the writer
	// sets it inside sync.Once.Do BEFORE close, so channel-close
	// release semantics make the value visible to the reader.
	var (
		selfExit   bool
		stopReason string
	)
	select {
	case sc.exitErr = <-waitErr:
		selfExit = true
	case <-sc.stopCh:
		stopReason = sc.stopReason
	case <-state.shutdownCh:
		stopReason = state.shutdownReason
	}
	if selfExit {
		if sc.exitErr != nil {
			log.Error("alpha", "%s exited: %v", name, sc.exitErr)
		} else {
			log.Info("alpha", "%s exited cleanly", name)
		}
		sc.lifecycle.Reach("stopped")
		if failfast {
			reason := fmt.Sprintf("failfast: service %s exited after ready", name)
			if sc.exitErr != nil {
				reason = fmt.Sprintf("%s: %v", reason, sc.exitErr)
			}
			log.Info("alpha", "%s, requesting alpha shutdown", reason)
			state.requestShutdown(reason)
		}
		return
	}
	// External stop. The reason was captured from whichever channel
	// fired (sc.stopCh ⇒ this service was singled out, state.shutdownCh
	// ⇒ alpha-wide shutdown). Surface it in the SIGTERM line so a
	// `tail -f alpha.log` immediately shows the cause instead of just
	// "SIGTERM api pgid=N" with no context — the question users asked
	// most often when debugging unexpected shutdowns.
	log.Info("alpha", "SIGTERM %s pgid=%d (reason: %s)", name, cmd.Process.Pid, stopReason)
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

// serviceEnv composes a child process env in the precedence documented
// at the top of this file (lowest → highest):
//
//   NewEnvironmentVariablesFromHost(allow) → toolchain → dotenv files → env
//
// Thin wrapper over zos.EnvironmentVariables.Join / .JoinFile so the
// spawn sites don't have to spell out the chain four times.
func serviceEnv(envFiles []string, env map[string]string, allow []string, toolchain map[string]string) []string {
	return zos.NewEnvironmentVariablesFromHost(allow).
		Join(toolchain).
		JoinFile(envFiles...).
		Join(env).
		Slice()
}

// phaseEnv is the explicit env map for one phase: the service-wide
// `env {}` base, then the phase overlay (build/runtime), then the
// `agent {}` overlay when alpha runs in --agent mode (each later layer
// overrides earlier keys).
func phaseEnv(svc *alphasfile.Service, phase zos.EnvironmentVariables, agent bool) zos.EnvironmentVariables {
	if svc.Runtime == nil {
		return nil
	}
	var ag zos.EnvironmentVariables
	if agent {
		ag = svc.Runtime.AgentEnv
	}
	return svc.Runtime.Env.Join(phase, ag)
}

func debuggerWrap(svc *alphasfile.Service, argv []string) []string {
	d := svc.Debugger
	if d == nil || !d.Enabled || !d.WrapRuntime {
		return argv
	}
	wrap := []string{
		"dlv", "exec",
		"--headless",
		fmt.Sprintf("--listen=127.0.0.1:%d", d.Port),
		"--api-version=2",
		"--accept-multiclient",
	}
	if !d.WaitForClient {
		wrap = append(wrap, "--continue")
	}
	if d.Log {
		wrap = append(wrap, "--log")
	}
	wrap = append(wrap, "--")
	return append(wrap, argv...)
}

func buildCmd(svc *alphasfile.Service, checkout string, envFiles []string, agent bool, sysenv []string, toolchain map[string]string) (*exec.Cmd, error) {
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
	case svc.Toolchain == alphasfile.ToolchainNode:
		// Node has no single-binary build artifact; with no explicit
		// runtime cmd, inference reads package.json (scripts.start →
		// main → bin) so the common case "just run my package.json"
		// needs zero config. checkout is the worktree dir; the project
		// root sits at checkout+exe.
		root := checkout
		if svc.Package != nil && strings.TrimSpace(svc.Package.Exe) != "" {
			root = filepath.Join(checkout, svc.Package.Exe)
		}
		argv, err := inferNodeRunCmd(root)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
		cmd = exec.Command(argv[0], append(argv[1:], svc.Flags()...)...)
	case binDir != "" && svc.Buildable():
		// Every service is built by zordon (git/src checkout or crate
		// cargo-install) into the out-of-tree bin dir; run it from there.
		// cwd = source checkout (when there is one) so relative config
		// paths resolve.
		argv := debuggerWrap(svc, append([]string{filepath.Join(binDir, name)}, svc.Flags()...))
		cmd = exec.Command(argv[0], argv[1:]...)
	default:
		argv := debuggerWrap(svc, append([]string{name}, svc.Flags()...))
		cmd = exec.Command(argv[0], argv[1:]...)
	}
	if checkout != "" {
		cmd.Dir = checkout
		// nodejs: cwd is the package.json directory (checkout + exe
		// offset), not the worktree root. npm / pnpm / yarn pick up
		// package.json by walking up, but if cwd is *below* it via a
		// monorepo offset they'd find the wrong one; setting cwd here
		// is the unambiguous version.
		if svc.Toolchain == alphasfile.ToolchainNode && svc.Package != nil && strings.TrimSpace(svc.Package.Exe) != "" {
			cmd.Dir = filepath.Join(checkout, svc.Package.Exe)
		}
	}
	var runEnv map[string]string
	if svc.Runtime != nil {
		runEnv = phaseEnv(svc, svc.Runtime.RunEnv, agent)
	}
	// ALWAYS set cmd.Env (even when no dotenv / no overlay): with cmd.Env
	// nil, exec inherits the full alpha process env — defeating the
	// closed-world whitelist. serviceEnv with empty sysenv returns no
	// host vars, which is the desired strictest default.
	cmd.Env = serviceEnv(envFiles, runEnv, sysenv, toolchain)
	return cmd, nil
}

// defaultBuild is the per-toolchain build run inside a fresh checkout when
// the service doesn't set `build = "..."`. For Go it honors `package`
// (the main package within the repo, default ".") so a repo whose main
// lives in cmd/foo needs no explicit build string. Output goes to the
// out-of-tree bin dir so a `dir` primary's worktree stays clean.
//
// dest is the checkout directory; nodejs reads package.json + lockfiles
// from there to pick the right package-manager invocation. Other
// toolchains ignore it.
func defaultBuild(svc *alphasfile.Service, name, binDir, dest string) string {
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
		// dlv breakpoints land on wrong lines (or "could not find file" for
		// inlined frames) without -N -l. Quoted because the build runs
		// through /bin/sh -c and the value has an embedded space.
		gcflags := ""
		if svc.Debugger != nil && svc.Debugger.Enabled {
			gcflags = `-gcflags='all=-N -l' `
		}
		return fmt.Sprintf("go build %s-o %q %s", gcflags, out, pkg)
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
	case alphasfile.ToolchainNode:
		// PM detection reads the project root (checkout + exe offset).
		// All four PMs install into ./node_modules in the project; we
		// never copy the artifact out (Node has no single binary).
		root := dest
		if svc.Package != nil && strings.TrimSpace(svc.Package.Exe) != "" {
			root = filepath.Join(dest, svc.Package.Exe)
		}
		pm := detectNodePM(root)
		return nodeInstallCmd(pm, root)
	}
	return ""
}

// nodePM is the result of PM detection — both the binary to invoke and
// whether a lockfile exists (which decides between `install` and the
// reproducible-mode flag).
type nodePM struct {
	bin         string // npm | pnpm | yarn | bun
	hasLockfile bool
}

// detectNodePM picks the package manager for a Node project root, in
// the order: bun.lock(b) → pnpm-lock.yaml → yarn.lock → package-lock.json
// → package.json#packageManager → npm fallback. The lockfile signal also
// flips the build into reproducible mode (`npm ci`, `--frozen-lockfile`).
func detectNodePM(root string) nodePM {
	if exists(filepath.Join(root, "bun.lockb")) || exists(filepath.Join(root, "bun.lock")) {
		return nodePM{bin: "bun", hasLockfile: true}
	}
	if exists(filepath.Join(root, "pnpm-lock.yaml")) {
		return nodePM{bin: "pnpm", hasLockfile: true}
	}
	if exists(filepath.Join(root, "yarn.lock")) {
		return nodePM{bin: "yarn", hasLockfile: true}
	}
	if exists(filepath.Join(root, "package-lock.json")) {
		return nodePM{bin: "npm", hasLockfile: true}
	}
	if pm := readPackageManagerField(filepath.Join(root, "package.json")); pm != "" {
		return nodePM{bin: pm}
	}
	return nodePM{bin: "npm"}
}

// readPackageManagerField parses package.json#packageManager (the
// Corepack-style pin, e.g. "pnpm@9.12.0+sha512:..."). Returns just the
// PM name (everything before '@'); the version part is handled by
// Corepack at runtime via the shim, not by zordon.
func readPackageManagerField(packageJSON string) string {
	b, err := os.ReadFile(packageJSON)
	if err != nil {
		return ""
	}
	var pj struct {
		PackageManager string `json:"packageManager"`
	}
	if err := json.Unmarshal(b, &pj); err != nil {
		return ""
	}
	pm := strings.TrimSpace(pj.PackageManager)
	if at := strings.IndexByte(pm, '@'); at > 0 {
		pm = pm[:at]
	}
	switch pm {
	case "npm", "pnpm", "yarn", "bun":
		return pm
	}
	return ""
}

// nodeInstallCmd builds the install shell line for the resolved PM.
// Reproducible mode (--frozen-lockfile / npm ci) when a lockfile is
// present — the common case for any committed project; first-time
// scaffold without a lockfile gets a plain install that creates one.
func nodeInstallCmd(pm nodePM, root string) string {
	cd := ""
	if root != "" {
		cd = fmt.Sprintf("cd %q && ", root)
	}
	switch pm.bin {
	case "npm":
		if pm.hasLockfile {
			return cd + "npm ci"
		}
		return cd + "npm install"
	case "pnpm":
		if pm.hasLockfile {
			return cd + "pnpm install --frozen-lockfile"
		}
		return cd + "pnpm install"
	case "yarn":
		// `--immutable` is yarn 2+; `--frozen-lockfile` is v1. yarn
		// accepts the wrong flag with a warning rather than a failure
		// on the version that doesn't own it, so pass both via shell
		// fallback: try immutable first (modern projects), then v1.
		if pm.hasLockfile {
			return cd + "yarn install --immutable 2>/dev/null || yarn install --frozen-lockfile"
		}
		return cd + "yarn install"
	case "bun":
		if pm.hasLockfile {
			return cd + "bun install --frozen-lockfile"
		}
		return cd + "bun install"
	}
	return cd + "npm install"
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// inferNodeRunCmd builds the runtime argv for a Node service that has
// no `runtime { cmd = [...] }`. Precedence — first hit wins:
//
//  1. package.json#scripts.start  →  <pm> start  (PM auto-detected)
//  2. package.json#main           →  node <main>
//  3. package.json#bin (string or single-entry map)  →  node <bin>
//
// Anything else is a hard error — we'd rather make the user write an
// explicit cmd than guess wrongly (e.g. picking `index.js` because it
// happens to exist).
func inferNodeRunCmd(root string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("read package.json (%s): %w — declare runtime { cmd = [...] }", root, err)
	}
	var pj struct {
		Scripts map[string]string `json:"scripts"`
		Main    string            `json:"main"`
		Bin     json.RawMessage   `json:"bin"`
	}
	if err := json.Unmarshal(b, &pj); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	if start := strings.TrimSpace(pj.Scripts["start"]); start != "" {
		pm := detectNodePM(root).bin
		return []string{pm, "start"}, nil
	}
	if m := strings.TrimSpace(pj.Main); m != "" {
		return []string{"node", m}, nil
	}
	if len(pj.Bin) > 0 {
		// `bin` may be a string ("./cli.js") or an object ({"foo": "./cli.js"}).
		var s string
		if err := json.Unmarshal(pj.Bin, &s); err == nil && strings.TrimSpace(s) != "" {
			return []string{"node", s}, nil
		}
		var m map[string]string
		if err := json.Unmarshal(pj.Bin, &m); err == nil && len(m) == 1 {
			for _, v := range m {
				return []string{"node", v}, nil
			}
		}
	}
	return nil, fmt.Errorf("package.json has no scripts.start, main, or single-entry bin — declare runtime { cmd = [...] }")
}

// prepareWorktree materializes the per-invocation checkout (or just
// resolves the live src dir for InPlace / returns "" for use-only) and
// returns the dir the subsequent build step should run in. Split out
// from the build step so the per-primary lock — which exists to keep
// concurrent `git clone --bare` / `git worktree add` on the same
// primary serial — only covers the git operations. Builds in distinct
// dest dirs are independent and run in parallel even when their
// services share a primary (a monorepo).
//
// Returns the checkout dir (empty for use-only services). Worktreeable
// = has a git or dir primary; crate/prebuilt services return "" here
// and let the subsequent build step (`cargo install <crate>` /
// `go install ...`) materialize the binary into bin dir.
func prepareWorktree(svc *alphasfile.Service, name string, stream *safeEncoder, log *zlog.Logger) (string, error) {
	if svc.Package == nil || !svc.Buildable() {
		return "", nil
	}
	runner := newPrepareRunner(name, stream, log)
	ctx := context.Background()

	switch {
	case svc.Package.InPlace:
		// src-only in the "main" worktree: use the live src tree as-is —
		// no git worktree add, no HEAD reset. Uncommitted edits just work.
		dest := ""
		if svc.Runtime != nil {
			dest = svc.Runtime.Dir
		}
		if dest == "" {
			return "", fmt.Errorf("in-place service %q has no resolved src dir", name)
		}
		log.Info("alpha", "prepare %s: in place %s (no worktree)", name, dest)
		return dest, nil
	case svc.Worktreeable():
		dest := ""
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
		return dest, nil
	default:
		log.Info("alpha", "prepare %s: use-only %s (install)", name, svc.Package.Install)
		return "", nil
	}
}

// prepareBuild runs the service's build step in dest (the worktree from
// prepareWorktree, or "" for use-only `cargo install` / `go install`).
// Safe to run concurrently across services: each writes to its own
// dest and bin dir; the per-primary lock is released BEFORE this step,
// so monorepo siblings build in parallel.
func prepareBuild(svc *alphasfile.Service, name, dest string, agent bool, state *alphaState, stream *safeEncoder, log *zlog.Logger) error {
	if svc.Package == nil || !svc.Buildable() {
		return nil
	}
	runner := newPrepareRunner(name, stream, log)
	ctx := context.Background()

	binDir := ""
	if svc.Runtime != nil {
		binDir = svc.Runtime.BinDir
	}
	if binDir != "" {
		if err := os.MkdirAll(binDir, 0o750); err != nil {
			return fmt.Errorf("mkdir bin dir: %w", err)
		}
	}

	// Explicit `build { cmd = [...] }` is exec'd as argv (no shell). With
	// no build block, the toolchain default runs via /bin/sh (it relies
	// on shell: env-prefixed `GOBIN=… go install`, `&&`, etc).
	var c *exec.Cmd
	if bc := svc.BuildCmd(); len(bc) > 0 {
		log.Info("alpha", "prepare %s: build (%v)", name, bc)
		c = exec.Command(bc[0], bc[1:]...)
	} else if def := strings.TrimSpace(defaultBuild(svc, name, binDir, dest)); def != "" {
		log.Info("alpha", "prepare %s: build (%s)", name, def)
		c = exec.Command("/bin/sh", "-c", def)
	}
	if c == nil {
		return nil
	}
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
	// Toolchain envs sit BELOW build.env — they're the initial
	// language environment (mise PATH/GOROOT/GOTOOLCHAIN etc.), the
	// build's explicit env overlays on top. Sharing this pin with
	// the runtime is what keeps build artifacts (vendor dirs,
	// generated code) ABI-compatible with the runtime that
	// consumes them.
	tcEnv, err := toolchainEnv(svc.Toolchain, state, log)
	if err != nil {
		return fmt.Errorf("build toolchain: %w", err)
	}
	// ALWAYS set Env (even empty) — nil would inherit alpha's full env
	// and undo the closed-world whitelist.
	c.Env = serviceEnv(nil, be, sysenv, tcEnv)
	// exec.Command resolved c.Path against alpha's os.Environ()
	// BEFORE we set c.Env. If the user passed a bare name (`go`,
	// `cargo`, etc.) it now points at whatever is on alpha's PATH
	// (typically the system install), not the mise-pinned binary
	// we just put on c.Env's PATH. Re-resolve now that env is final.
	if err := relookupPath(c); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if err := runner(ctx, c); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	return nil
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
