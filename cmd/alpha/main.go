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
	config         *alphasfile.Alphasfile
	running        map[string]*exec.Cmd
}

func (s *alphaState) configure(path string, af *alphasfile.Alphasfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alphasfilePath = path
	s.config = af
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
	state.configure(req.Configure.AlphasfilePath, req.Configure.Alphasfile)
	services := req.Configure.Alphasfile.All()
	log.Info("alpha", "configured from %s (%d services, failfast=%v), starting bringup", req.Configure.AlphasfilePath, len(services), failfast)

	stream := newSafeEncoder(enc)
	defer stream.Close()

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

func buildCmd(svc *alphasfile.Service, repoDir string) (*exec.Cmd, error) {
	name := svc.Name()
	if name == "" {
		return nil, errors.New("service has no name")
	}
	var cmd *exec.Cmd
	switch {
	case svc.Ruby != nil && strings.TrimSpace(svc.Ruby.Run) != "":
		fields := strings.Fields(svc.Ruby.Run)
		cmd = exec.Command(fields[0], append(fields[1:], svc.Flags()...)...)
	default:
		cmd = exec.Command(name, svc.Flags()...)
	}
	switch {
	case svc.Runtime != nil && svc.Runtime.Dir != "":
		cmd.Dir = svc.Runtime.Dir
	case repoDir != "":
		cmd.Dir = repoDir
	}
	return cmd, nil
}

// prepare materializes the source checkout and runs the per-service install
// command for services that ship as a git tree to be invoked from a checkout
// (currently: Ruby with Git). Output is streamed back to zordon as log events
// under stream="prepare" so the user sees clone/install progress live.
//
// Returns the resolved repo dir (empty when no preparation was needed) so the
// caller can set exec.Cmd.Dir on the run command.
func prepare(svc *alphasfile.Service, name string, stream *safeEncoder, log *zlog.Logger) (string, error) {
	if svc.Ruby == nil || svc.Ruby.Git == "" {
		return "", nil
	}
	runner := newPrepareRunner(name, stream, log)

	log.Info("alpha", "prepare %s: ensuring source %s", name, svc.Ruby.Git)
	info, err := source.Ensure(context.Background(), svc.Ruby.Git, svc.Branch(), runner)
	if err != nil {
		return "", fmt.Errorf("ensure source: %w", err)
	}

	if install := strings.TrimSpace(svc.Install()); install != "" {
		fields := strings.Fields(install)
		log.Info("alpha", "prepare %s: install %s", name, install)
		c := exec.Command(fields[0], fields[1:]...)
		c.Dir = info.RepoDir
		if err := runner(context.Background(), c); err != nil {
			return "", fmt.Errorf("install: %w", err)
		}
	}
	return info.RepoDir, nil
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
