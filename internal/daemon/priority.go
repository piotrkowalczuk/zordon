package daemon

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"syscall"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

const DefaultNice = 10

func LowerSelfPriority() error {
	pid := os.Getpid()
	if cur, err := ProcessNice(pid); err == nil && cur >= DefaultNice {
		return nil
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		err := syscall.Setpriority(syscall.PRIO_PGRP, pgid, DefaultNice)
		if runtime.GOOS != "linux" {
			return err
		}
	}
	if runtime.GOOS == "linux" {
		return lowerLinuxThreadsPriority(pid)
	}
	return syscall.Setpriority(syscall.PRIO_PROCESS, pid, DefaultNice)
}

func ProcessNice(pid int) (int, error) {
	if pid <= 0 {
		panic("daemon.ProcessNice: pid must be > 0")
	}
	raw, err := syscall.Getpriority(syscall.PRIO_PROCESS, pid)
	if err != nil {
		return 0, err
	}
	// Linux's raw SYS_GETPRIORITY returns 20 - nice (1..40) so negative nice
	// values do not collide with -errno; Darwin calls libc getpriority directly.
	if runtime.GOOS == "linux" {
		return 20 - raw, nil
	}
	return raw, nil
}

// On Linux PRIO_PROCESS applies to a single kernel thread (TID), not the whole
// multithreaded Go process; walk /proc/self/task twice so non-pgrp-leader
// processes and any Go runtime thread cloned concurrently during startup are
// also lowered.
func lowerLinuxThreadsPriority(pid int) error {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, DefaultNice)
	var firstErr error
	applied := false
	for range 2 {
		entries, err := zfs.ReadDir("/proc/self/task")
		if err != nil {
			if !applied {
				return syscall.Setpriority(syscall.PRIO_PROCESS, pid, DefaultNice)
			}
			return firstErr
		}
		for _, e := range entries {
			tid, convErr := strconv.Atoi(e.Name())
			if convErr != nil || tid <= 0 {
				continue
			}
			applied = true
			if setErr := syscall.Setpriority(syscall.PRIO_PROCESS, tid, DefaultNice); setErr != nil && !errors.Is(setErr, syscall.ESRCH) && firstErr == nil {
				firstErr = setErr
			}
		}
	}
	if !applied {
		return syscall.Setpriority(syscall.PRIO_PROCESS, pid, DefaultNice)
	}
	return firstErr
}
