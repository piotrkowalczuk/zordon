package daemon

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// Alive reports whether a process with pid exists on the host.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Terminate sends SIGTERM to pid. A non-positive pid or an already-exited
// process (ESRCH) is a no-op.
func Terminate(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("SIGTERM pid %d: %w", pid, err)
	}
	return nil
}

// Kill sends SIGKILL to pid. A non-positive pid or an already-exited
// process (ESRCH) is a no-op.
func Kill(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("SIGKILL pid %d: %w", pid, err)
	}
	return nil
}

// ReapGroup drives a process group to exit: SIGTERM(-pgid), wait up to grace for
// exited to close, then SIGKILL(-pgid). Returns nil once the group is gone.
//
// exited is closed by the caller when the supervised process has been waited;
// after a SIGKILL the process dies and that wait returns, so the final receive
// does not block. A group that is already gone (ESRCH) is a no-op success.
//
// pgid MUST be a real group id (> 0): kill(-pgid) targets the whole group, and
// a non-positive pgid would broadcast — so the caller passing one is misuse.
func ReapGroup(pgid int, grace time.Duration, exited <-chan struct{}) error {
	if pgid <= 0 {
		panic(fmt.Sprintf("daemon.ReapGroup: non-positive pgid %d", pgid))
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("SIGTERM group %d: %w", pgid, err)
	}
	select {
	case <-exited:
		return nil
	case <-time.After(grace):
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("SIGKILL group %d: %w", pgid, err)
		}
		<-exited
		return nil
	}
}
