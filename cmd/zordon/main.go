package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/control"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

func main() {
	rootFlags := ff.NewFlagSet("zordon")
	verbose := rootFlags.Bool('v', "verbose", "verbose logging")
	agent := rootFlags.BoolLong("agent", "machine-friendly output: '<ms-since-start> <src> <LEVEL> <msg>'")
	rootCmd := &ff.Command{
		Name:  "zordon",
		Usage: "zordon [FLAGS] <SUBCOMMAND>",
		Flags: rootFlags,
	}

	// start
	startFlags := ff.NewFlagSet("start").SetParent(rootFlags)
	startAlphaBin := startFlags.StringLong("alpha", "alpha", "alpha binary (name on $PATH or absolute path)")
	startAlphaLog := startFlags.StringLong("alpha-log", "/tmp/alpha.log", "log file for alpha")
	startTimeout := startFlags.DurationLong("timeout", 30*time.Second, "max wait for alpha to become ready")
	startFailfast := startFlags.BoolLong("failfast", "abort bringup and shut down alpha on first service failure")
	startCmd := &ff.Command{
		Name:      "start",
		Usage:     "zordon start [FLAGS]",
		ShortHelp: "ensure alpha is running and push the Alphasfile config",
		Flags:     startFlags,
		Exec: func(ctx context.Context, args []string) error {
			return runStart(ctx, zlog.New(os.Stderr, *agent), *startAlphaBin, *startAlphaLog, *startTimeout, *startFailfast, *verbose)
		},
	}

	// status
	statusFlags := ff.NewFlagSet("status").SetParent(rootFlags)
	statusCmd := &ff.Command{
		Name:      "status",
		Usage:     "zordon status",
		ShortHelp: "query the running alpha for its state",
		Flags:     statusFlags,
		Exec: func(ctx context.Context, args []string) error {
			return runStatus(ctx, zlog.New(os.Stderr, *agent))
		},
	}

	// stop
	stopFlags := ff.NewFlagSet("stop").SetParent(rootFlags)
	stopCmd := &ff.Command{
		Name:      "stop",
		Usage:     "zordon stop",
		ShortHelp: "ask the running alpha to shut down",
		Flags:     stopFlags,
		Exec: func(ctx context.Context, args []string) error {
			return runStop(ctx, zlog.New(os.Stderr, *agent))
		},
	}

	rootCmd.Subcommands = append(rootCmd.Subcommands, startCmd, statusCmd, stopCmd)

	err := rootCmd.ParseAndRun(context.Background(), os.Args[1:])
	switch {
	case errors.Is(err, ff.ErrHelp), errors.Is(err, ff.ErrNoExec):
		fmt.Fprintln(os.Stderr, ffhelp.Command(rootCmd))
		os.Exit(0)
	case err != nil:
		zlog.New(os.Stderr, *agent).Error("zordon", "%v", err)
		os.Exit(1)
	}
}

// walkUp climbs from cwd toward / looking for an Alphasfile.
func walkUp() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "Alphasfile")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no Alphasfile found in cwd or any parent")
		}
		dir = parent
	}
}

func loadAlphasfile() (string, *alphasfile.Alphasfile, error) {
	path, err := walkUp()
	if err != nil {
		return "", nil, err
	}
	stateDir, err := control.StateDir(path)
	if err != nil {
		return path, nil, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return path, nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	af, err := alphasfile.Open(path, stateDir)
	if err != nil {
		return path, nil, err
	}
	return path, af, nil
}

func runStart(ctx context.Context, log *zlog.Logger, alphaBin, alphaLog string, timeout time.Duration, failfast, verbose bool) error {
	log.Warn("zordon", "Rangers, you must act swiftly, the development environment is in grave danger!")
	afPath, af, err := loadAlphasfile()
	if err != nil {
		return err
	}
	sock, err := control.SocketPath(afPath)
	if err != nil {
		return err
	}
	log.Info("zordon", "alphasfile=%s socket=%s failfast=%v", afPath, sock, failfast)

	ctxStart, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Try to talk to an existing alpha first.
	if c, err := control.Dial(sock, 200*time.Millisecond); err == nil {
		c.Close()
		log.Info("zordon", "alpha already running, pushing config")
		return pushConfigure(ctxStart, log, sock, afPath, af, failfast)
	}

	// No alpha running; spawn one.
	if err := spawnAlpha(alphaBin, alphaLog, sock, timeout, verbose, log); err != nil {
		return err
	}
	if err := control.WaitListening(ctxStart, sock); err != nil {
		return fmt.Errorf("waiting for alpha socket: %w", err)
	}
	return pushConfigure(ctxStart, log, sock, afPath, af, failfast)
}

func pushConfigure(ctx context.Context, log *zlog.Logger, sock, afPath string, af *alphasfile.Alphasfile, failfast bool) error {
	log.Info("alpha", "Understood, Zordon!")

	conn, err := control.Dial(sock, 1*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	enc := protocol.NewEncoder(conn)
	dec := protocol.NewDecoder(conn)

	if err := enc.Write(&protocol.Request{
		Op: protocol.OpConfigure,
		Configure: &protocol.ConfigureArgs{
			AlphasfilePath: afPath,
			Alphasfile:     af,
			Failfast:       failfast,
		},
	}); err != nil {
		return fmt.Errorf("send configure: %w", err)
	}
	log.Info("zordon", "config pushed (%d services, failfast=%v), streaming bringup logs", len(af.All()), failfast)

	var failed []string
	for {
		var ev protocol.Event
		if err := dec.Read(&ev); err != nil {
			return fmt.Errorf("recv event: %w", err)
		}
		switch ev.Kind {
		case protocol.EventLog:
			log.Service(ev.Service, ev.Stream, ev.Line)
		case protocol.EventServiceStart:
			log.Info("alpha", "service start: %s", ev.Service)
		case protocol.EventServiceReady:
			log.Info("alpha", "service ready: %s", ev.Service)
		case protocol.EventServiceFail:
			log.Error("alpha", "service FAILED: %s: %s", ev.Service, ev.Error)
			failed = append(failed, ev.Service)
		case protocol.EventDone:
			if len(failed) > 0 {
				return fmt.Errorf("bringup finished with %d failure(s): %s", len(failed), strings.Join(failed, ", "))
			}
			log.Info("zordon", "alpha ready, detaching")
			return nil
		case protocol.EventError:
			return fmt.Errorf("alpha: %s", ev.Error)
		default:
			log.Info("zordon", "alpha sent unknown event kind=%q", ev.Kind)
		}
	}
}

func spawnAlpha(alphaBin, alphaLog, sock string, timeout time.Duration, verbose bool, log *zlog.Logger) error {
	logf := func(format string, a ...any) { log.Info("zordon", format, a...) }
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}

	cmd := exec.Command(alphaBin, "run", "--socket", sock, "--log-file", alphaLog)
	cmd.ExtraFiles = []*os.File{readyW}
	cmd.Env = append(os.Environ(), "ZORDON_READY_FD="+strconv.Itoa(2+len(cmd.ExtraFiles)))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		readyR.Close()
		readyW.Close()
		return fmt.Errorf("start alpha: %w", err)
	}
	logf("spawned alpha pid=%d log=%s", cmd.Process.Pid, alphaLog)
	readyW.Close()

	readyCh := make(chan struct{})
	errCh := make(chan error, 1)
	go readHandshake(readyR, readyCh, errCh, logf, verbose)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case <-readyCh:
		logf("alpha ready (pid=%d)", cmd.Process.Pid)
		return nil
	case err := <-errCh:
		_ = cmd.Process.Kill()
		return err
	case err := <-waitCh:
		if err == nil {
			return errors.New("alpha exited before READY")
		}
		return fmt.Errorf("alpha exited before READY: %w", err)
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return fmt.Errorf("timed out after %s waiting for READY", timeout)
	}
}

func readHandshake(r *os.File, readyCh chan<- struct{}, errCh chan<- error, logf func(string, ...any), verbose bool) {
	defer r.Close()
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			if verbose {
				logf("alpha: %s", line)
			}
			continue
		}
		switch key {
		case "STATUS":
			logf("alpha status: %s", value)
		case "READY":
			if value == "1" {
				close(readyCh)
				return
			}
		case "ERROR":
			errCh <- fmt.Errorf("alpha reported error: %s", value)
			return
		default:
			if verbose {
				logf("alpha %s=%s", key, value)
			}
		}
	}
	if err := s.Err(); err != nil {
		errCh <- fmt.Errorf("read handshake: %w", err)
		return
	}
	errCh <- errors.New("alpha closed handshake without READY")
}

func runStatus(ctx context.Context, log *zlog.Logger) error {
	_ = log // status output is structured stdout, not log-style
	afPath, _, err := loadAlphasfile()
	if err != nil {
		return err
	}
	sock, err := control.SocketPath(afPath)
	if err != nil {
		return err
	}
	resp, err := control.Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpState})
	if err != nil {
		if control.IsNotRunning(err) {
			return fmt.Errorf("no alpha running for %s", afPath)
		}
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("alpha: %s", resp.Error)
	}
	if resp.State == nil {
		return errors.New("alpha returned empty state")
	}
	st := resp.State
	fmt.Printf("alpha pid=%d started=%s\n", st.PID, st.StartedAt)
	fmt.Printf("alphasfile=%s\n", st.AlphasfilePath)
	if len(st.Services) == 0 {
		fmt.Println("services: (none configured yet)")
		return nil
	}
	runningByName := make(map[string]int, len(st.Running))
	for _, r := range st.Running {
		runningByName[r.Name] = r.PID
	}
	fmt.Printf("services (%d):\n", len(st.Services))
	for _, s := range st.Services {
		state := "stopped"
		if pid, ok := runningByName[s.Name()]; ok {
			state = fmt.Sprintf("running pid=%d", pid)
		}
		fmt.Printf("  - [%s] %s — %s\n", s.Toolchain, s.Name(), state)
	}
	return nil
}

func runStop(ctx context.Context, log *zlog.Logger) error {
	afPath, _, err := loadAlphasfile()
	if err != nil {
		return err
	}
	sock, err := control.SocketPath(afPath)
	if err != nil {
		return err
	}
	resp, err := control.Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpShutdown})
	if err != nil {
		if control.IsNotRunning(err) {
			log.Info("zordon", "alpha is not running")
			return nil
		}
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("alpha: %s", resp.Error)
	}
	log.Info("alpha", "shutdown requested")
	return nil
}
