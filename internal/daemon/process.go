package daemon

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// Process makes a started command's exit selectable like a context: Done()
// closes when it exits, and Err() is the Wait result. It hides the one goroutine
// a blocking exec.Cmd.Wait would otherwise force into every caller's select, so
// a supervisor body reads as a single `select { case <-proc.Done(): ... }`.
type Process struct {
	done chan struct{}
	err  error
}

// Wait backgrounds the reap of cmd (which MUST already be Started) and returns
// immediately with a Process whose Done() closes on exit. NOT blocking — the
// name mirrors the exec.Cmd.Wait it stands in for.
func Wait(cmd *exec.Cmd) *Process {
	p := &Process{done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p
}

// Done closes once the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err is the exec.Cmd.Wait result (nil on a clean exit). Read it only after
// Done() is observed closed — that close establishes the happens-before.
func (p *Process) Err() error { return p.err }

func RunGroup(ctx context.Context, grace time.Duration, cmd *exec.Cmd) error {
	if ctx == nil {
		panic("daemon.RunGroup: nil context")
	}
	if cmd == nil {
		panic("daemon.RunGroup: nil cmd")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return err
	}
	proc := Wait(cmd)
	select {
	case <-proc.Done():
		return proc.Err()
	case <-ctx.Done():
		_ = ReapGroup(cmd.Process.Pid, grace, proc.Done())
		return ctx.Err()
	}
}
