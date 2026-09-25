package ztest

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestAssertSystem_ready(t *testing.T) {
	rec := &recorder{TB: t}
	AssertSystem(rec, Need{name: "ok", ready: func() bool { return true }})
	if rec.failed {
		t.Fatalf("a ready system must pass, got %q", rec.msg)
	}
}

func TestAssertSystem_listsEveryMissingNeed(t *testing.T) {
	rec := &recorder{TB: t}
	AssertSystem(rec,
		Need{name: "one", why: "first reason", ready: func() bool { return false }},
		Need{name: "two", why: "second reason", ready: func() bool { return false }},
	)
	if !rec.failed {
		t.Fatal("missing needs must fail the test")
	}
	for _, want := range []string{"system not ready", "  - one: first reason", "  - two: second reason"} {
		if !strings.Contains(rec.msg, want) {
			t.Errorf("missing %q in %q", want, rec.msg)
		}
	}
}

func TestAssertSystem_alwaysChecksGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	rec := &recorder{TB: t}
	AssertSystem(rec)
	if !rec.failed || !strings.Contains(rec.msg, "  - git: ") {
		t.Fatalf("git must be required even with no needs given, got failed=%v %q", rec.failed, rec.msg)
	}
}

func TestToolchain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	if Toolchain(home).ready() {
		t.Fatal("no pre-placed mise and no cargo must not be ready")
	}
	if err := zfs.EnsureDir(filepath.Join(home, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := zfs.AtomicWrite(filepath.Join(home, "bin", "mise"), []byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	if !Toolchain(home).ready() {
		t.Fatal("a pre-placed mise must be ready")
	}
}

// recorder captures Fatalf instead of ending the enclosing test.
type recorder struct {
	testing.TB
	failed bool
	msg    string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}
