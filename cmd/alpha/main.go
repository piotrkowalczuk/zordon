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
	"github.com/piotrkowalczuk/zordon/internal/control"
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
	services       map[string]*serviceCtx
	readiness      map[string]string // service name -> readiness state ("probing", "ready", "failed")
	files          []string          // absolute paths of file{} outputs to unlink on shutdown

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
// lifecycle. The supervisor goroutine is the sole owner of cmd.Wait — no
// other goroutine calls it — so the "exec: Wait was already called"
// race that the old two-watchers design had can't happen.
//
//	stopCh closed → "stop just this service" (reconfigure path)
//	shutdownCh    → "stop all services" (failfast / signal)
//	cmd self-exit → supervisor handles it (failfast trigger if `ready`)
//	done closed   → supervisor has finished; safe to read exitErr
type serviceCtx struct {
	name     string
	cmd      *exec.Cmd
	stopCh   chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	// ready flips when waitReady succeeded; supervisor reads it on
	// self-exit to decide whether to trigger failfast.
	ready atomic.Bool
	// exitErr is the result of cmd.Wait. Written by supervisor before
	// closing done; the close is the happens-before edge for any reader
	// that has already received <-done.
	exitErr error
}

func (sc *serviceCtx) requestStop() {
	sc.stopOnce.Do(func() { close(sc.stopCh) })
}

// supervise owns cmd.Wait for the lifetime of the service. It waits for
// whichever comes first — external stop or self-exit — and reaps the
// process cleanly. close(sc.done) is deferred so any goroutine selecting
// on <-sc.done is unblocked exactly once, no matter which path was taken.
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
		// Only "exited after ready" warrants triggering global failfast;
		// "exited before ready" is bringup's failure to surface through
		// the EventServiceFail path bringupService already drives.
		if sc.ready.Load() && failfast {
			log.Info("alpha", "failfast: %s exited after ready, requesting alpha shutdown", sc.name)
			state.requestShutdown()
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

// shutdownAll closes every supervisor's stopCh and waits for their done
// channels. The supervisor itself handles SIGTERM/grace/SIGKILL, so this
// function carries no timer logic of its own. Files generated by
// materializeFiles are removed at the end.
func (s *alphaState) shutdownAll(log *zlog.Logger) {
	s.mu.Lock()
	services := s.services
	s.services = nil
	files := s.files
	s.files = nil
	s.mu.Unlock()

	for _, sc := range services {
		sc.requestStop()
	}
	for _, sc := range services {
		<-sc.done
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
func (s *alphaState) stopServices(names map[string]bool) {
	s.mu.Lock()
	picked := make([]*serviceCtx, 0, len(names))
	for name := range names {
		if sc := s.services[name]; sc != nil {
			picked = append(picked, sc)
			delete(s.services, name)
		}
	}
	s.mu.Unlock()

	for _, sc := range picked {
		sc.requestStop()
	}
	for _, sc := range picked {
		<-sc.done
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

	sc := &serviceCtx{
		name:   name,
		cmd:    cmd,
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
	state.addService(sc)
	state.setReadiness(name, "probing")
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
		sc.ready.Store(true)
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
