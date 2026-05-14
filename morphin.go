package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codegangsta/cli"
	"go.uber.org/zap"
)

func morphin(ctx *cli.Context) error {
	af, err := openAlphasfile(alphasFile)
	if err != nil {
		logger.Fatal(err.Error())
	}

	logger.Warn("Rangers, you must act swiftly, the development environment is in grave danger!", zap.String(keySubsystem, "zordon"))

	if ctx.Bool("install") {
		toolchains := map[string]bool{}
		for _, s := range af.All() {
			l := serviceLogger(logger, s)
			toolchains[s.Toolchain] = true
			var src sourceInfo
			if s.NeedsSource() {
				var err error
				src, err = ensureSource(s, l)
				if err != nil {
					l.Fatal(fmt.Sprintf("%s source error", s.Name), zap.Error(err))
				}
			}
			var install *exec.Cmd
			if custom := s.CustomInstall(); custom == "" {
				install = s.InstallCmd(src)
			} else {
				fields := strings.Fields(custom)
				install = exec.Command(fields[0], fields[1:]...)
				install.Env = os.Environ()
				install.Dir = src.repoDir
			}
			if err := run(install, s, l); err != nil {
				l.Fatal(fmt.Sprintf("%s installation error: %s", s.Name, err.Error()))
			}

			l.Info(fmt.Sprintf("%s!!!", strings.ToUpper(s.Name)))
		}
		warnIfBinNotInPath(toolchains)
	}

	al := logger.With(zap.String(keySubsystem, "alpha"))

	// Pre-flight cleanup: find and kill any orphaned processes from previous runs.
	if err := killAll(al); err != nil {
		al.Error("pre-flight cleanup failed", zap.Error(err))
	}

	defer func() {
		if r := recover(); r != nil {
			killAll(al)
			fmt.Println("Recovered in f", r)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	var wg sync.WaitGroup
	for _, r := range af.All() {
		<-time.After(1 * time.Second)
		wg.Add(1)
		go func(s *Service) {
			defer wg.Done()
			morphRanger(s, logger)
		}(r)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-c:
		al.Info("interrupt received, killing services")
		killAll(al)
		<-done
	case <-done:
		al.Info("all services have stopped, shutting down")
	}
	return nil
}

func morphRanger(s *Service, l *zap.Logger) {
	rl := serviceLogger(l, s)
	afAbs, _ := filepath.Abs(alphasFile)
	for {
		cmd := exec.Command(s.Name, s.Flags()...)
		cmd.Dir = s.Dir
		cmd.Env = append(os.Environ(),
			"ZORDON=1",
			"ZORDON_SERVICE="+s.Name,
			"ZORDON_ALPHASFILE="+afAbs,
			"ZORDON_PPID="+strconv.Itoa(os.Getpid()),
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		if err := run(cmd, s, rl); err != nil {
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				rl.Error("service will be restarted because of error", zap.Error(err))
				time.Sleep(1 * time.Second)
				continue
			}

			rl.Error("service has stoped with error", zap.Error(err))
			return
		}
	}
}

func run(c *exec.Cmd, s *Service, l *zap.Logger) error {
	stderr, err := c.StderrPipe()
	if err != nil {
		return err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}

	if err = c.Start(); err != nil {
		return err
	}

	if err := savePID(s.Name, c.Process.Pid); err != nil {
		l.Error("failed to save PID", zap.Error(err))
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sc(stdout, s, l) }()
	go func() { defer wg.Done(); sc(stderr, s, l) }()
	wg.Wait()

	return c.Wait()
}

func sc(rc io.Reader, s *Service, l *zap.Logger) {
	in := bufio.NewScanner(rc)
ScanLoop:
	for in.Scan() {
		switch s.Log {
		case "json":
			if !bytes.HasPrefix(in.Bytes(), []byte("{")) {
				l.Info(in.Text())
				continue ScanLoop
			}
			tmp := map[string]any{}
			if err := json.Unmarshal(in.Bytes(), &tmp); err != nil {
				l.Info(in.Text(), zap.String(keyError, err.Error()))
				continue ScanLoop
			}
			fields := make([]zap.Field, 0, len(tmp))
			msg := ""
			for k, v := range tmp {
				if (k == "msg" || k == "message") && msg == "" {
					if s, ok := v.(string); ok {
						msg = s
						continue
					}
				}
				fields = append(fields, zap.Any(k, v))
			}
			l.Info(msg, fields...)
		default:
			l.Info(in.Text())
		}
	}
	if err := in.Err(); err != nil {
		l.Error("scan error", zap.String(keySubsystem, "alpha"), zap.Error(err))
	}
}
