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
	configHash     string
	parentDotenv   []string // file-level dotenv paths inherited from federation parents
	config         *alphasfile.Alphasfile
	running        map[string]*exec.Cmd
	readiness      map[string]string // service name -> readiness state ("probing", "ready", "failed")
	files          []string          // absolute paths of file{} outputs to unlink on shutdown
}

func (s *alphaState) configure(path, hash string, parentDotenv []string, af *alphasfile.Alphasfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alphasfilePath = path
	s.configHash = hash
	s.parentDotenv = parentDotenv
	s.config = af
	s.readiness = make(map[string]string)
}

func (s *alphaState) addFile(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files = append(s.files, p)
}

func (s *alphaState) addProcess(name string, cmd *exec.Cmd) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = make(map[string]*exec.Cmd)
	}
	s.running[name] = cmd
}

func (s *alphaState) setReadiness(name, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readiness == nil {
		s.readiness = make(map[string]string)
	}
	s.readiness[name] = state
}

func (s *alphaState) removeProcess(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, name)
	delete(s.readiness, name)
}

func (s *alphaState) snapshot() *protocol.StateInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info := &protocol.StateInfo{
		PID:            os.Getpid(),
		AlphasfilePath: s.alphasfilePath,
		StartedAt:      s.startedAt.Format(time.RFC3339),
		ConfigHash:     s.configHash,
	}
	if s.config != nil {
		info.Services = s.config.All()
		info.Dotenv = s.config.Dotenv
	}
	for name, cmd := range s.running {
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
		info.Running = append(info.Running, protocol.ServiceStatus{
			Name:      name,
			PID:       pid,
			Readiness: s.readiness[name],
		})
	}
	return info
}

func (s *alphaState) shutdownAll(grace time.Duration, log *zlog.Logger) {
	s.mu.Lock()
	procs := s.running
	s.running = nil
	files := s.files
	s.files = nil
	s.mu.Unlock()

	if len(procs) == 0 {
		return
	}

	var wg sync.WaitGroup
	for name, cmd := range procs {
		if cmd.Process == nil {
			continue
		}
		wg.Add(1)
		go func(name string, cmd *exec.Cmd) {
			defer wg.Done()
			log.Info("alpha", "SIGTERM %s pid=%d", name, cmd.Process.Pid)
			_ = cmd.Process.Signal(syscall.SIGTERM)

			// Wait for the process to exit or for the grace period to expire.
			done := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(done)
			}()

			select {
			case <-done:
				log.Info("alpha", "%s exited after SIGTERM", name)
			case <-time.After(grace):
				log.Info("alpha", "SIGKILL %s pid=%d (grace period expired)", name, cmd.Process.Pid)
				_ = cmd.Process.Kill()
				<-done
			}
		}(name, cmd)
	}
	wg.Wait()

	for _, p := range files {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Error("alpha", "unlink %s: %v", p, err)
		} else {
			log.Info("alpha", "removed file %s", p)
		}
	}
}

// stopServices stops only the named running services (SIGTERM, grace,
// SIGKILL) and drops them from s.running, leaving every other service
// untouched. Used on reconfigure so a changed service restarts without
// disturbing the ones whose code/manifest didn't change. Generated files
// are left in place (cleaned only on full shutdownAll).
func (s *alphaState) stopServices(names map[string]bool, grace time.Duration, log *zlog.Logger) {
	s.mu.Lock()
	procs := map[string]*exec.Cmd{}
	for name := range names {
		if cmd := s.running[name]; cmd != nil {
			procs[name] = cmd
			delete(s.running, name)
		}
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for name, cmd := range procs {
		if cmd.Process == nil {
			continue
		}
		wg.Add(1)
		go func(name string, cmd *exec.Cmd) {
			defer wg.Done()
			log.Info("alpha", "SIGTERM %s pid=%d (reconfigure)", name, cmd.Process.Pid)
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
				log.Info("alpha", "%s exited after SIGTERM", name)
			case <-time.After(grace):
				log.Info("alpha", "SIGKILL %s pid=%d (grace expired)", name, cmd.Process.Pid)
				_ = cmd.Process.Kill()
				<-done
			}
		}(name, cmd)
	}
	wg.Wait()
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

	state := &alphaState{startedAt: time.Now()}
	cfg := bringupConfig{stabilization: stabilization, shutdownGrace: shutdownGrace}

	if err := signalReady(); err != nil {
		log.Error("alpha", "ready signal: %v", err)
	}

	shutdownCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go acceptLoop(ln, state, cfg, log, shutdownCh)

	select {
	case sig := <-sigCh:
		log.Info("alpha", "received %s, shutting down children", sig)
	case <-shutdownCh:
		log.Info("alpha", "shutdown requested via control socket")
	}
	state.shutdownAll(shutdownGrace, log)
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

func acceptLoop(ln net.Listener, state *alphaState, cfg bringupConfig, log *zlog.Logger, shutdownCh chan<- struct{}) {
	var once sync.Once
	requestShutdown := func() { once.Do(func() { close(shutdownCh) }) }

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Error("alpha", "accept: %v", err)
			return
		}
		go handleConn(conn, state, cfg, log, requestShutdown)
	}
}

func handleConn(conn net.Conn, state *alphaState, cfg bringupConfig, log *zlog.Logger, requestShutdown func()) {
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
		handleConfigure(&req, state, cfg, enc, log, requestShutdown)
	case protocol.OpState:
		_ = enc.Write(&protocol.Response{OK: true, State: state.snapshot()})
	case protocol.OpShutdown:
		log.Info("alpha", "shutdown op received")
		requestShutdown()
		_ = enc.Write(&protocol.Response{OK: true})
	default:
		_ = enc.Write(&protocol.Response{Error: fmt.Sprintf("unknown op: %q", req.Op)})
	}
}

func handleConfigure(req *protocol.Request, state *alphaState, cfg bringupConfig, enc *protocol.Encoder, log *zlog.Logger, requestShutdown func()) {
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
	for name := range state.running {
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
			if !running[name] { continue }
			
			newSvc, exists := newSvcs[name]
			if !exists || fmt.Sprintf("%v", oldSvc) != fmt.Sprintf("%v", newSvc) {
				toStop[name] = true
			}
		}
	}
	
	if len(toStop) > 0 {
		state.stopServices(toStop, cfg.shutdownGrace, log)
	}

	state.configure(req.Configure.AlphasfilePath, req.Configure.ConfigHash, req.Configure.ParentDotenv, newConfig)
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
		ok := bringupService(svc, state, cfg, stream, log, failfast, requestShutdown)
		if !ok && failfast {
			log.Info("alpha", "failfast: %s did not come up, aborting remaining bringup", svc.Name())
			state.shutdownAll(cfg.shutdownGrace, log)
			stream.Send(&protocol.Event{Kind: protocol.EventError, Error: fmt.Sprintf("failfast: %s failed during bringup", svc.Name())})
			requestShutdown()
			return
		}
	}
	stream.Send(&protocol.Event{Kind: protocol.EventDone})
	log.Info("alpha", "bringup complete")
}

func bringupService(svc *alphasfile.Service, state *alphaState, cfg bringupConfig, stream *safeEncoder, log *zlog.Logger, failfast bool, requestShutdown func()) bool {
	name := svc.Name()
	log.Info("alpha", "bringup service=%s toolchain=%s", name, svc.Toolchain)
	stream.Send(&protocol.Event{Kind: protocol.EventServiceStart, Service: name})

	repoDir, err := prepare(svc, name, stream, log)
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

	cmd, err := buildCmd(svc, repoDir, envFiles)
	if err != nil {
		log.Error("alpha", "build cmd %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		return false
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		return false
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		return false
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		log.Error("alpha", "start %s: %v", name, err)
		stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: fmt.Sprintf("Ayiyiyiyi! %s", err)})
		return false
	}
	state.addProcess(name, cmd)
	state.setReadiness(name, "probing")
	log.Info("alpha", "started %s pid=%d argv=%v", name, cmd.Process.Pid, cmd.Args)

	go streamLines(name, "stdout", stdout, stream, log)
	go streamLines(name, "stderr", stderr, stream, log)

	var (
		sent          sync.Once
		bringupFailed atomic.Bool
		resultCh      = make(chan bool, 1)
	)
	probeCtx, cancelProbe := context.WithCancel(context.Background())
	defer cancelProbe()

	go func() {
		err := cmd.Wait()
		cancelProbe()
		sent.Do(func() {
			bringupFailed.Store(true)
			state.setReadiness(name, "failed")
			msg := "exited before ready"
			if err != nil {
				msg = err.Error()
			}
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: msg})
			resultCh <- false
		})
		state.removeProcess(name)
		if err != nil {
			log.Error("alpha", "%s exited: %v", name, err)
		} else {
			log.Info("alpha", "%s exited cleanly", name)
		}
		if !bringupFailed.Load() && failfast {
			log.Info("alpha", "failfast: %s exited after ready, requesting alpha shutdown", name)
			requestShutdown()
		}
	}()

	waitReady(probeCtx, svc, name, cfg.stabilization, stream, log, &sent, resultCh, state)
	return <-resultCh
}

// waitReady picks readiness strategy: probe if configured, else time-based
// stabilization. Sends EventServiceReady or EventServiceFail via sent.Do so
// the cmd.Wait watcher can race it cleanly.
func waitReady(ctx context.Context, svc *alphasfile.Service, name string, stabilization time.Duration, stream *safeEncoder, log *zlog.Logger, sent *sync.Once, resultCh chan<- bool, state *alphaState) {
	p := readinessOf(svc)
	if p == nil {
		select {
		case <-time.After(stabilization):
		case <-ctx.Done():
			return // goroutine already handled failure
		}
		sent.Do(func() {
			state.setReadiness(name, "ready")
			stream.Send(&protocol.Event{Kind: protocol.EventServiceReady, Service: name})
			resultCh <- true
		})
		return
	}

	log.Info("alpha", "probing %s readiness", name)
	err := p.Wait(ctx, func(ok bool, reason string) {
		if ok {
			log.Info("alpha", "probe ok %s (%s)", name, reason)
		} else {
			log.Info("alpha", "probe miss %s (%s)", name, reason)
		}
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return // cmd.Wait already won the race
		}
		sent.Do(func() {
			state.setReadiness(name, "failed")
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: "readiness: " + err.Error()})
			resultCh <- false
		})
		return
	}
	sent.Do(func() {
		state.setReadiness(name, "ready")
		stream.Send(&protocol.Event{Kind: protocol.EventServiceReady, Service: name})
		resultCh <- true
	})
}

func readinessOf(svc *alphasfile.Service) *probe.Probe {
	if svc == nil || svc.Runtime == nil {
		return nil
	}
	return svc.Runtime.Readiness
}

func streamLines(name, kind string, r io.Reader, stream *safeEncoder, log *zlog.Logger) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		line := s.Text()
		log.Service(name, kind, line)
		stream.Send(&protocol.Event{
			Kind:    protocol.EventLog,
			Service: name,
			Stream:  kind,
			Line:    line,
		})
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

func buildCmd(svc *alphasfile.Service, checkout string, envFiles []string) (*exec.Cmd, error) {
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
	var envMap map[string]string
	if svc.Runtime != nil {
		envMap = svc.Runtime.Env
	}
	if len(envFiles) > 0 || len(envMap) > 0 {
		cmd.Env = serviceEnv(envFiles, envMap)
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
		return "bundle install --path vendor/bundle"
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
func prepare(svc *alphasfile.Service, name string, stream *safeEncoder, log *zlog.Logger) (string, error) {
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

	build := strings.TrimSpace(svc.Build())
	if build == "" {
		build = defaultBuild(svc, name, binDir)
	}
	if build != "" {
		log.Info("alpha", "prepare %s: build (%s)", name, build)
		c := exec.Command("/bin/sh", "-c", build)
		c.Dir = dest // "" ⇒ alpha's cwd; fine for `cargo install <crate>`
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
