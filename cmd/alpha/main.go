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
	config         *alphasfile.Alphasfile
	running        map[string]*exec.Cmd
	files          []string // absolute paths of file{} outputs to unlink on shutdown
}

func (s *alphaState) configure(path, hash string, af *alphasfile.Alphasfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alphasfilePath = path
	s.configHash = hash
	s.config = af
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

func (s *alphaState) removeProcess(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, name)
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
	}
	for name, cmd := range s.running {
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
		info.Running = append(info.Running, protocol.ServiceStatus{Name: name, PID: pid})
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

	for name, cmd := range procs {
		if cmd.Process == nil {
			continue
		}
		log.Info("alpha", "SIGTERM %s pid=%d", name, cmd.Process.Pid)
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	time.Sleep(grace)
	for name, cmd := range procs {
		if cmd.Process == nil {
			continue
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			continue
		}
		log.Info("alpha", "SIGKILL %s pid=%d", name, cmd.Process.Pid)
		_ = cmd.Process.Kill()
	}

	for _, p := range files {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Error("alpha", "unlink %s: %v", p, err)
		} else {
			log.Info("alpha", "removed file %s", p)
		}
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
	state.configure(req.Configure.AlphasfilePath, req.Configure.ConfigHash, req.Configure.Alphasfile)
	services := req.Configure.Alphasfile.All()
	files := req.Configure.Alphasfile.AllFiles()
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

	cmd, err := buildCmd(svc, repoDir)
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
	log.Info("alpha", "started %s pid=%d argv=%v", name, cmd.Process.Pid, cmd.Args)

	go streamLines(name, "stdout", stdout, stream, log)
	go streamLines(name, "stderr", stderr, stream, log)

	var sent sync.Once
	bringupFailed := atomic.Bool{}
	probeCtx, cancelProbe := context.WithCancel(context.Background())
	defer cancelProbe()

	go func() {
		err := cmd.Wait()
		cancelProbe()
		readyBeforeExit := true
		sent.Do(func() {
			readyBeforeExit = false
			bringupFailed.Store(true)
			msg := "exited before ready"
			if err != nil {
				msg = err.Error()
			}
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: msg})
		})
		state.removeProcess(name)
		if err != nil {
			log.Error("alpha", "%s exited: %v", name, err)
		} else {
			log.Info("alpha", "%s exited cleanly", name)
		}
		if readyBeforeExit && failfast {
			log.Info("alpha", "failfast: %s exited after ready, requesting alpha shutdown", name)
			requestShutdown()
		}
	}()

	waitReady(probeCtx, svc, name, cfg.stabilization, stream, log, &sent, &bringupFailed)
	return !bringupFailed.Load()
}

// waitReady picks readiness strategy: probe if configured, else time-based
// stabilization. Sends EventServiceReady or EventServiceFail via sent.Do so
// the cmd.Wait watcher can race it cleanly.
func waitReady(ctx context.Context, svc *alphasfile.Service, name string, stabilization time.Duration, stream *safeEncoder, log *zlog.Logger, sent *sync.Once, failed *atomic.Bool) {
	p := readinessOf(svc)
	if p == nil {
		select {
		case <-time.After(stabilization):
		case <-ctx.Done():
			return
		}
		sent.Do(func() {
			stream.Send(&protocol.Event{Kind: protocol.EventServiceReady, Service: name})
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
			failed.Store(true)
			stream.Send(&protocol.Event{Kind: protocol.EventServiceFail, Service: name, Error: "readiness: " + err.Error()})
		})
		return
	}
	sent.Do(func() {
		stream.Send(&protocol.Event{Kind: protocol.EventServiceReady, Service: name})
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

func buildCmd(svc *alphasfile.Service, checkout string) (*exec.Cmd, error) {
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
	case checkout != "" && binDir != "":
		// Worktree service, no explicit command: run the built binary from
		// the (out-of-tree) bin dir, with cwd = source checkout so relative
		// config paths still resolve.
		cmd = exec.Command(filepath.Join(binDir, name), svc.Flags()...)
	default:
		// Crate / prebuilt: resolve from $PATH.
		cmd = exec.Command(name, svc.Flags()...)
	}
	if checkout != "" {
		cmd.Dir = checkout
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
	switch svc.Toolchain {
	case alphasfile.ToolchainGo:
		pkg := "."
		if svc.Package != nil && strings.TrimSpace(svc.Package.Exe) != "" {
			pkg = svc.Package.Exe
		}
		if pkg != "." {
			return fmt.Sprintf("go build -C %q -o %q .", pkg, out)
		}
		return fmt.Sprintf("go build -o %q .", out)
	case alphasfile.ToolchainRust:
		return "cargo build --release"
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
func prepare(svc *alphasfile.Service, name string, stream *safeEncoder, log *zlog.Logger) (string, error) {
	if !svc.Worktreeable() || svc.Package == nil {
		return "", nil
	}
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
	runner := newPrepareRunner(name, stream, log)
	ctx := context.Background()

	log.Info("alpha", "prepare %s: ensuring primary (%s)", name, p.Kind)
	if err := p.Ensure(ctx, runner); err != nil {
		return "", fmt.Errorf("ensure primary: %w", err)
	}

	wt := os.Getenv("ZORDON_WORKTREE")
	if wt == "" {
		wt = "main"
	}
	log.Info("alpha", "prepare %s: worktree -> %s (branch zordon/%s)", name, dest, wt)
	if err := p.AddWorktree(ctx, dest, "zordon/"+wt, runner); err != nil {
		return "", fmt.Errorf("git worktree: %w", err)
	}

	binDir := ""
	if svc.Runtime != nil {
		binDir = svc.Runtime.BinDir
	}
	if binDir != "" {
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir bin dir: %w", err)
		}
	}
	build := strings.TrimSpace(svc.Build())
	if build == "" {
		build = defaultBuild(svc, name, binDir)
	}
	if build != "" {
		log.Info("alpha", "prepare %s: build (%s)", name, build)
		c := exec.Command("/bin/sh", "-c", build)
		c.Dir = dest
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
