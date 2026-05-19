// Command tommy is a transparent process wrapper whose single job is to
// guarantee a spawned service never outlives whatever spawned it. alpha
// runs `tommy -- <service argv>` instead of the service directly; tommy
// execs the service and then enforces:
//
//	the spawner (alpha) dies  ->  tommy reaps the service, then exits.
//
// The hard requirement driving the design: alpha can be SIGKILLed or
// OOM-killed, in which case alpha runs NO cleanup code. The orphan
// guarantee therefore cannot depend on alpha signalling anything. It
// rests instead on what the kernel does unconditionally when a process
// dies — close its file descriptors and reparent its children — so the
// two parent-death detectors need zero cooperation from a dying alpha:
//
//   - parent pipe EOF (primary): alpha holds the write end of a pipe
//     and passes the read end to tommy via $TOMMY_PARENT_FD. The kernel
//     closes that write end the instant alpha's process table entry
//     goes away — SIGKILL and OOM included — so tommy's blocked Read
//     returns immediately. The only detector that is both instant and
//     SIGKILL/OOM-proof.
//   - getppid() reparent poll (belt-and-braces): when tommy's direct
//     parent dies tommy is reparented to launchd/init (pid 1), so a
//     changed getppid() means the spawner is gone. Works with no FD
//     plumbing at all.
//
// Process-group model: alpha starts tommy with Setpgid, so tommy is a
// fresh group leader (pgid == tommy's pid). tommy does NOT put the
// service in its own group — the service inherits tommy's group. That
// is deliberate:
//
//   - alpha's existing supervision/registry/orphan-reaper all signal
//     kill(-pgid); because tommy and the service share that group a
//     single group signal reaches BOTH. alpha needs zero changes to
//     how it stops or reaps a service.
//   - a group-wide SIGTERM/SIGKILL from alpha hits the service
//     DIRECTLY, so for an alpha-initiated stop tommy doesn't even need
//     to forward anything.
//   - even if tommy itself is SIGKILLed, the service is still in the
//     registry-tracked group, so the next alpha boot's reaper covers
//     it. Defense in depth.
//
// Because the service shares tommy's group, tommy must NOT let an
// alpha-issued group SIGTERM kill the wrapper out from under the
// service. tommy CATCHES (does not Ignore) the terminal signals:
// signal.Ignore sets SIG_IGN, which exec() PRESERVES into the service —
// the service would then ignore SIGTERM too, breaking graceful stop for
// everything. signal.Notify installs a handler, which exec() resets to
// SIG_DFL in the child, so the service keeps normal signal behavior. A
// terminal signal received by tommy is treated as "wind down": the
// service already got its own copy via the group kill, so tommy just
// reaps it (escalating to SIGKILL after grace).
//
// tommy also ignores SIGPIPE. Once the spawner dies, the stdout/stderr
// pipe alpha handed tommy has no reader; Go's default is to kill the
// process on EPIPE to fd 1/2, which would take tommy down — before it
// reaps the service — the instant it logged anything. Draining SIGPIPE
// turns those writes into harmless errors.
//
// darwin has no PR_SET_PDEATHSIG (the Linux one-syscall version of all
// this), which is why the mechanism is built explicitly.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func main() {
	grace := flag.Duration("shutdown-grace", 2*time.Second,
		"time the service gets after SIGTERM before SIGKILL (mirrors alpha's --shutdown-grace)")
	parentFD := flag.Int("parent-fd", -1,
		"inherited read-end fd of a pipe the spawner holds open; EOF means the spawner died (defaults to $TOMMY_PARENT_FD)")
	flag.Parse()

	argv := flag.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "tommy: usage: tommy [flags] -- <command> [args...]")
		os.Exit(2)
	}

	if *parentFD < 0 {
		if v := os.Getenv("TOMMY_PARENT_FD"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*parentFD = n
			}
		}
	}

	os.Exit(run(argv, *grace, *parentFD))
}

func run(argv []string, grace time.Duration, parentFD int) int {
	// Drain SIGPIPE so a write to the (reader-gone) log pipe after the
	// spawner dies can't kill tommy before it reaps the service.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)

	// CATCH the terminal signals (see package comment): keeps the
	// service's own signal behavior intact via exec() reset to SIG_DFL,
	// while ensuring a group-wide SIGTERM doesn't drop tommy and orphan
	// the service.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)

	// Keep the parent-death fd to OURSELVES. alpha passes it via
	// exec.ExtraFiles, which deliberately clears O_CLOEXEC so tommy
	// inherits it — but tommy then exec()s the service, which would
	// inherit it too (an unintended fd leak into the user's process).
	// Marking it close-on-exec drops it from the service while tommy
	// keeps it for its whole life (tommy never exec()s itself; the
	// flag only fires on the service's exec).
	if parentFD >= 0 {
		syscall.CloseOnExec(parentFD)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	// Inherit tommy's stdio verbatim. alpha attaches its stdout/stderr
	// capture (pipes or a pty) to the process it spawns (tommy); passing
	// them straight through keeps tommy invisible to log streaming.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// No SysProcAttr: the service inherits tommy's process group on
	// purpose (see the package comment) — do NOT Setpgid here.

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "tommy: start %q: %v\n", argv[0], err)
		return 127
	}

	// tommy's own pgid. alpha started tommy with Setpgid, so tommy is
	// the group leader and this group contains exactly tommy + the
	// service (+ anything the service forks). Safe to signal wholesale.
	// No blanket "kill the group on any return" defer: tommy is IN this
	// group, so that would SIGKILL tommy itself and clobber clean
	// exit-code propagation. Every non-error return below has already
	// reaped the service explicitly.
	pgid := syscall.Getpgrp()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	parentGone := make(chan struct{})
	var once sync.Once
	gone := func() { once.Do(func() { close(parentGone) }) }
	if pf := os.NewFile(uintptr(parentFD), "tommy-parent"); parentFD >= 0 && pf != nil {
		go watchParentPipe(pf, gone)
	}
	go watchParentReparent(os.Getppid(), gone)

	select {
	case err := <-waitCh:
		// Service exited on its own — possibly because alpha's group
		// kill signalled it directly. Propagate its status.
		return exitCode(err)

	case <-parentGone:
		// Spawner died WITHOUT signalling the service (SIGKILL/OOM):
		// no one will stop it but us.
		fmt.Fprintln(os.Stderr, "tommy: spawner gone, reaping service")
		return reap(pgid, grace, waitCh)

	case sig := <-sigCh:
		// A terminal signal hit the group. The service got its own
		// copy directly (SIG_DFL) and is already on its way out; tommy
		// just sees it down and escalates if it lingers.
		fmt.Fprintf(os.Stderr, "tommy: got %v, reaping service\n", sig)
		return reap(pgid, grace, waitCh)
	}
}

// reap drives the service to exit: SIGTERM the group, wait up to grace,
// then SIGKILL the group. The SIGKILL hits tommy too (shared group), so
// the post-kill return is best-effort — by then everything is gone and
// the exit code no longer matters (the spawner that wanted it is dead).
func reap(pgid int, grace time.Duration, waitCh <-chan error) int {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case err := <-waitCh:
		return exitCode(err)
	case <-time.After(grace):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-waitCh
		return 137 // 128 + SIGKILL, the conventional shell code
	}
}

// watchParentPipe blocks on the inherited read end. A blocking pipe Read
// only returns when data arrives or every write end is closed; we hold
// no write end, so the only wakeup is the spawner's last write end
// closing — which the kernel does on ANY spawner death, SIGKILL/OOM
// included. Stray data (a spawner misusing the fd) is ignored; only
// EOF/error counts as death.
func watchParentPipe(pf *os.File, gone func()) {
	buf := make([]byte, 64)
	for {
		_, err := pf.Read(buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "tommy: parent fd read: %v\n", err)
			}
			gone()
			return
		}
	}
}

// watchParentReparent polls getppid(). When tommy's direct parent dies,
// the kernel reparents tommy to launchd/init (pid 1), so a ppid that no
// longer matches the one captured at startup means the spawner is gone.
// 250ms keeps teardown prompt at negligible cost, and this needs no fd
// plumbing — it is the safety net when the pipe fd was never passed.
func watchParentReparent(origPPID int, gone func()) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		if os.Getppid() != origPPID {
			gone()
			return
		}
	}
}

// exitCode maps a cmd.Wait() error to a process exit status: 0 on clean
// exit, the child's own code on a non-zero exit, 128+signal when the
// child died to a signal, and 1 for anything else (e.g. wait failure).
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
