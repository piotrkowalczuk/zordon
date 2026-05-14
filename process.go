package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"go.uber.org/zap"
)

const (
	zordonDir = ".zordon"
)

func savePID(name string, pid int) error {
	f, err := openPIDFile(name)
	if err != nil {
		// If it already exists, we want to overwrite it.
		// openPIDFile uses O_EXCL, so we need to handle that.
		fpath := filepath.Join(zordonDir, name+".pid")
		f, err = os.OpenFile(fpath, os.O_WRONLY|os.O_TRUNC, 0666)
		if err != nil {
			return err
		}
	}
	defer f.Close()

	if _, err := f.WriteString(strconv.Itoa(pid)); err != nil {
		return err
	}
	return nil
}

func openPIDFile(name string) (*os.File, error) {
	fpath := filepath.Join(zordonDir, name+".pid")
	if _, err := os.Stat(filepath.Dir(fpath)); os.IsNotExist(err) {
		if err := os.Mkdir(filepath.Dir(fpath), 0777); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(fpath, os.O_EXCL|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		return nil, fmt.Errorf("cannot create %s.pid: %v", name, err)
	}
	return f, nil
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil
}

func killAll(l *zap.Logger) error {
	entries, err := scanZordonProcs()
	if err != nil {
		return err
	}

	myPID := os.Getpid()
	for _, e := range entries {
		// Kill if:
		// 1. It belongs to this instance (standard shutdown)
		// 2. Its parent (ZORDON_PPID) is no longer alive (orphan from previous crash)
		isOrphan := !isProcessAlive(e.ppid)
		if e.ppid != myPID && !isOrphan {
			continue
		}

		p, err := os.FindProcess(e.pid)
		if err != nil {
			continue
		}

		if err := p.Kill(); err != nil {
			// Ignore error if process already exited
			continue
		}

		msg := "process %s (%d) has been killed"
		if isOrphan {
			msg = "orphaned process %s (%d) from zordon pid %d has been cleaned up"
			l.Info(fmt.Sprintf(msg, e.service, e.pid, e.ppid))
		} else {
			l.Info(fmt.Sprintf(msg, e.service, e.pid))
		}
	}

	// Also clean up any stale PID files if the directory exists.
	if _, err := os.Stat(zordonDir); err == nil {
		os.RemoveAll(zordonDir)
	}

	return nil
}

// getProcess gets a Process from a pid and checks that the
// process is actually running. If the process
// is not running, then getProcess returns a nil
// Process and the error ErrNotRunning.
func getProcess(pid int) (*os.Process, error) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil, err
	}

	// try to check if the process is actually running by sending
	// it signal 0.
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return p, nil
	}
	if err == syscall.ESRCH {
		return nil, errors.New("zordon: service is not running")
	}
	return nil, errors.New("server running but inaccessible")
}
