package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLowerSelfPriority_inheritedAcrossProcessTree(t *testing.T) {
	switch os.Getenv("ZORDON_TEST_PRIORITY_ROLE") {
	case "leader", "standalone":
		runPriorityHelperChild()
		os.Exit(0)
	case "grandchild":
		nice, err := ProcessNice(os.Getpid())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(nice)
		os.Exit(0)
	}

	parentNice, err := ProcessNice(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessNice(self): %v", err)
	}
	wantNice := max(parentNice, DefaultNice)

	cases := map[string]struct {
		role    string
		setpgid bool
	}{
		"group-leader": {role: "leader", setpgid: true},
		"standalone":   {role: "standalone", setpgid: false},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestLowerSelfPriority_inheritedAcrossProcessTree$")
			cmd.Env = append(os.Environ(), "ZORDON_TEST_PRIORITY_ROLE="+c.role)
			if c.setpgid {
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helper %s failed: %v\noutput:\n%s", c.role, err, out)
			}
			selfNice, grandchildNice := parseTwoInts(t, string(out))
			if selfNice != wantNice {
				t.Errorf("child nice = %d, want %d", selfNice, wantNice)
			}
			if grandchildNice != wantNice {
				t.Errorf("grandchild nice = %d, want %d", grandchildNice, wantNice)
			}

			afterParentNice, err := ProcessNice(os.Getpid())
			if err != nil {
				t.Fatalf("ProcessNice(parent after %s): %v", c.role, err)
			}
			if afterParentNice != parentNice {
				t.Fatalf("parent nice mutated from %d to %d", parentNice, afterParentNice)
			}
		})
	}
}

func TestRunGroup_cancelsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	cmd := exec.Command("/bin/sh", "-c", "echo ready; sleep 30")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- RunGroup(ctx, 2*time.Second, cmd)
	}()

	go func() {
		buf := make([]byte, 16)
		_, _ = stdout.Read(buf)
		close(started)
	}()

	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunGroup err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunGroup did not terminate process group after cancel")
	}
}

func runPriorityHelperChild() {
	if err := LowerSelfPriority(); err != nil {
		fmt.Fprintf(os.Stderr, "LowerSelfPriority: %v\n", err)
		os.Exit(1)
	}
	selfNice, err := ProcessNice(os.Getpid())
	if err != nil {
		fmt.Fprintf(os.Stderr, "ProcessNice(self): %v\n", err)
		os.Exit(1)
	}
	grand := exec.Command(os.Args[0], "-test.run=^TestLowerSelfPriority_inheritedAcrossProcessTree$")
	grand.Env = append(os.Environ(), "ZORDON_TEST_PRIORITY_ROLE=grandchild")
	grand.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := grand.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "grandchild: %v (%s)\n", err, out)
		os.Exit(1)
	}
	fmt.Printf("%d %s\n", selfNice, strings.TrimSpace(string(out)))
}

func parseTwoInts(t *testing.T, s string) (int, int) {
	t.Helper()
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) != 2 {
		t.Fatalf("expected 2 ints, got %q", s)
	}
	a, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("parse first int %q: %v", fields[0], err)
	}
	b, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("parse second int %q: %v", fields[1], err)
	}
	return a, b
}
