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
	*startFailfast = true // default to true
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

	// sudo
	sudoFlags := ff.NewFlagSet("sudo").SetParent(rootFlags)
	sudoCmd := &ff.Command{
		Name:      "sudo",
		Usage:     "zordon sudo",
		ShortHelp: "run the idempotent privileged hooks for the whole federation chain",
		Flags:     sudoFlags,
		Exec: func(ctx context.Context, args []string) error {
			return runSudo(ctx, zlog.New(os.Stderr, *agent))
		},
	}

	// worktree
	wtFlags := ff.NewFlagSet("worktree").SetParent(rootFlags)
	wtCmd := &ff.Command{
		Name:      "worktree",
		Usage:     "zordon worktree <create|list|rm> [name]",
		ShortHelp: "manage parallel worktrees (isolated state/ports over the same Alphasfile)",
		Flags:     wtFlags,
		Exec: func(ctx context.Context, args []string) error {
			return runWorktree(ctx, zlog.New(os.Stderr, *agent), args)
		},
	}

	rootCmd.Subcommands = append(rootCmd.Subcommands, startCmd, statusCmd, stopCmd, sudoCmd, wtCmd)

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

func pushConfigure(ctx context.Context, log *zlog.Logger, sock, afPath, hash string, parentDotenv []string, af *alphasfile.Alphasfile, failfast bool) error {
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
			ConfigHash:     hash,
			ParentDotenv:   parentDotenv,
		},
	}); err != nil {
		return fmt.Errorf("send configure: %w", err)
	}
	
	// Clear the deadline for the streaming phase (bringup can take minutes)
	_ = conn.SetDeadline(time.Time{})
	
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

func spawnAlpha(alphaBin, alphaLog, sock string, timeout time.Duration, verbose bool, log *zlog.Logger, extraEnv map[string]string) error {
	logf := func(format string, a ...any) { log.Info("zordon", format, a...) }
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}

	cmd := exec.Command(alphaBin, "run", "--socket", sock, "--log-file", alphaLog)
	cmd.ExtraFiles = []*os.File{readyW}
	cmd.Env = append(os.Environ(), "ZORDON_READY_FD="+strconv.Itoa(2+len(cmd.ExtraFiles)))
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
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

	// Status reports the whole federation chain, not just the invocation.
	levels, err := resolveChain(ctx)
	if err != nil {
		return err
	}

	anyRunning := false
	for i, lv := range levels {
		if i > 0 {
			fmt.Println()
		}
		marker := ""
		if lv.isInvocation {
			marker = fmt.Sprintf(" (invocation, worktree=%s)", lv.inv.Worktree)
		}
		fmt.Printf("# [%s] %s%s\n", lv.inv.Hash, lv.afPath, marker)

		if lv.state == nil {
			fmt.Println("  alpha: not running")
			continue
		}
		anyRunning = true
		st := lv.state
		fmt.Printf("  alpha pid=%d started=%s\n", st.PID, st.StartedAt)
		if len(st.Services) == 0 {
			fmt.Println("  services: (none configured yet)")
			continue
		}
		runningByName := make(map[string]protocol.ServiceStatus, len(st.Running))
		for _, r := range st.Running {
			runningByName[r.Name] = r
		}
		fmt.Printf("  services (%d):\n", len(st.Services))
		for _, s := range st.Services {
			state := "stopped"
			if status, ok := runningByName[s.Name()]; ok {
				health := ""
				if p := s.Runtime.Readiness; p != nil {
					// Perform a live, single-shot probe.
					pctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
					err := p.Check(pctx)
					cancel()

					if err == nil {
						health = " [ready]"
					} else {
						health = fmt.Sprintf(" [unhealthy: %v]", err)
					}
				}
				state = fmt.Sprintf("running pid=%d%s", status.PID, health)
			}
			fmt.Printf("    - [%s] %s — %s\n", s.Toolchain, s.Name(), state)
			if s.Runtime != nil && s.Runtime.Print != "" {
				// Plain text: the value is the composed (interpolated)
				// string; the terminal linkifies any URL itself.
				fmt.Printf("        %s\n", s.Runtime.Print)
			}
		}
	}
	if !anyRunning {
		return errors.New("no alpha running in the federation chain")
	}
	return nil
}

func runStop(ctx context.Context, log *zlog.Logger) error {
	// Stop only the invocation (leaf); parents are shared infra. We still
	// need the chain walk to derive the leaf's socket (its hash depends on
	// the resolved parent context).
	levels, err := resolveChain(ctx)
	if err != nil {
		return err
	}
	var leaf *level
	for _, lv := range levels {
		if lv.isInvocation {
			leaf = lv
		}
	}
	if leaf == nil {
		return errors.New("no invocation level in chain")
	}
	resp, err := control.Roundtrip(ctx, leaf.inv.SocketPath(), &protocol.Request{Op: protocol.OpShutdown})
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
	log.Info("alpha", "shutdown requested (worktree=%s)", leaf.inv.Worktree)
	return nil
}
