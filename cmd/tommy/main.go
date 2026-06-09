// Command tommy is a transparent process wrapper whose single job is to
// guarantee a spawned service never outlives whatever spawned it. alpha runs
// `tommy -- <service argv>` instead of the service directly; tommy execs the
// service and, when its spawner (alpha) dies, reaps the service so it never
// orphans — even when alpha is SIGKILLed or OOM-killed and runs NO cleanup.
//
// The lifecycle plumbing — parent-death detection (pipe-EOF + getppid),
// terminal-signal handling, SIGPIPE draining, and the reap escalation
// (SIGTERM → grace → SIGKILL) — lives in internal/daemon. tommy only wires it:
// AsLongAsAlive(parentFD) ends tommy when alpha dies, and on that koniec the
// body ReapGroups the service.
//
// Process-group model (tommy-specific, NOT in daemon): alpha starts tommy with
// Setpgid, so tommy leads a fresh group. tommy does NOT put the service in its
// own group — the service inherits tommy's. That is deliberate:
//
//   - alpha's supervision/registry/orphan-reaper all signal kill(-pgid);
//     because tommy and the service share the group a single group signal
//     reaches BOTH, so alpha needs zero changes to how it stops or reaps a
//     service, and an alpha-initiated group stop hits the service directly.
//   - even if tommy itself is SIGKILLed, the service is still in the
//     registry-tracked group, so the next alpha boot's reaper covers it.
//
// Because the service shares tommy's group, tommy must not let an alpha-issued
// group SIGTERM kill the wrapper out from under the service before it reaps —
// so it ends on the wider terminal set (TERM/INT/HUP/QUIT) via daemon.New, all
// caught (not Ignored) so the exec'd service keeps SIG_DFL behavior.
//
// darwin has no PR_SET_PDEATHSIG (the Linux one-syscall version of all this),
// which is why the mechanism is built explicitly in internal/parentwatch.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/daemon"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func main() {
	grace := flag.Duration("shutdown-grace", 2*time.Second,
		"time the service gets after SIGTERM before SIGKILL (mirrors alpha's --shutdown-grace)")
	var parentFD zfs.FileDescriptor
	flag.Var(&parentFD, "parent-fd",
		"inherited read-end fd of a pipe the spawner holds open; EOF means the spawner died (0 ⇒ no pipe, getppid poll only)")
	// stdlib flag has no env-var binding, but the canonical source for this fd
	// IS an env var (alpha sets TOMMY_PARENT_FD when spawning tommy because
	// tommy's argv slot is owned by the user's service). Honor it as a fallback
	// ONLY when --parent-fd wasn't passed.
	if v := os.Getenv("TOMMY_PARENT_FD"); v != "" {
		_ = parentFD.Set(v) // best-effort: bad value falls through to argv flag
	}
	flag.Parse()

	argv := flag.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "tommy: usage: tommy [flags] -- <command> [args...]")
		os.Exit(2)
	}

	os.Exit(run(argv, *grace, parentFD))
}

func run(argv []string, grace time.Duration, parentFD zfs.FileDescriptor) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	// Inherit tommy's stdio verbatim: alpha attaches its stdout/stderr capture
	// to the process it spawns (tommy); passing it straight through keeps tommy
	// invisible to log streaming.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// No SysProcAttr: the service inherits tommy's process group on purpose
	// (see the package comment) — do NOT Setpgid here.

	// proc is set once the service is started — inside the daemon body, so the
	// parent-fd is CLOEXEC before the fork. nil ⇒ it never started.
	var proc *daemon.Process

	// tommy ends on the wider terminal set: it shares the service's group, so a
	// group-wide SIGHUP/SIGQUIT must not drop the wrapper before it reaps.
	d := daemon.New(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	runErr := d.Run(context.Background(), func(ctx context.Context) error {
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start %q: %w", argv[0], err)
		}
		// alpha started tommy with Setpgid, so tommy leads this group and it
		// holds exactly tommy + the service (+ anything the service forks).
		pgid := syscall.Getpgrp()
		proc = daemon.Wait(cmd)
		select {
		case <-proc.Done():
			// Service exited on its own — possibly because alpha's group kill
			// signalled it directly. Propagate its status.
			return proc.Err()
		case <-ctx.Done():
			// Spawner gone (SIGKILL/OOM) or a terminal signal: no one else will
			// stop the service, so reap the group.
			fmt.Fprintln(os.Stderr, "tommy: winding down, reaping service")
			return daemon.ReapGroup(pgid, grace, proc.Done())
		}
	}, daemon.AsLongAsAlive(parentFD))
	if proc == nil {
		fmt.Fprintf(os.Stderr, "tommy: %v\n", runErr)
		return 127
	}
	return exitCode(proc.Err())
}

// exitCode maps a cmd.Wait() error to a process exit status: 0 on clean exit,
// the child's own code on a non-zero exit, 128+signal when the child died to a
// signal, and 1 for anything else (e.g. wait failure).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		return ee.ExitCode()
	}
	return 1
}
