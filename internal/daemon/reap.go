package daemon

import (
	"fmt"
	"syscall"
	"time"
)

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
