package daemon

import "os/exec"

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
