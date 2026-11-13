package daemon

import (
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
		return syscall.Setpriority(syscall.PRIO_PGRP, pgid, DefaultNice)
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
// multithreaded Go process; walk /proc/self/task when alpha is not a pgrp leader.
func lowerLinuxThreadsPriority(pid int) error {
	entries, err := zfs.ReadDir("/proc/self/task")
	if err != nil {
		return syscall.Setpriority(syscall.PRIO_PROCESS, pid, DefaultNice)
	}
	var firstErr error
	applied := false
	for _, e := range entries {
		tid, convErr := strconv.Atoi(e.Name())
		if convErr != nil || tid <= 0 {
			continue
		}
		applied = true
		if setErr := syscall.Setpriority(syscall.PRIO_PROCESS, tid, DefaultNice); setErr != nil && firstErr == nil {
			firstErr = setErr
		}
	}
	if !applied {
		return syscall.Setpriority(syscall.PRIO_PROCESS, pid, DefaultNice)
	}
	return firstErr
}
