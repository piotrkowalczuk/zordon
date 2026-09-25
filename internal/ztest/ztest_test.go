package ztest

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestAssertSystem_extraNeedMissing(t *testing.T) {
	rec := &recorder{TB: t}
	AssertSystem(rec, Need{name: "extra", why: "extra reason", ready: func() bool { return false }})
	if !rec.failed || !strings.Contains(rec.msg, "  - extra: extra reason") {
		t.Fatalf("a missing extra need must fail the test, got failed=%v %q", rec.failed, rec.msg)
	}
}

func TestAssertSystem_alwaysChecksGitAndMise(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	rec := &recorder{TB: t}
	AssertSystem(rec)
	if !rec.failed {
		t.Fatal("an empty PATH must fail the test")
	}
	if !strings.Contains(rec.msg, "  - git: ") {
		t.Errorf("git must always be checked, got %q", rec.msg)
	}
	if zfs.Missing(filepath.Join(Home(t), "bin", "mise")) && !strings.Contains(rec.msg, "  - mise: ") {
		t.Errorf("mise must always be checked, got %q", rec.msg)
	}
}

func TestMiseNeed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	if miseNeed(home).ready() {
		t.Fatal("no placed mise and no cargo must not be ready")
	}
	if err := zfs.EnsureDir(filepath.Join(home, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := zfs.AtomicWrite(filepath.Join(home, "bin", "mise"), []byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	if !miseNeed(home).ready() {
		t.Fatal("a placed mise must be ready")
	}
}

func TestHome(t *testing.T) {
	if got := Home(t); filepath.Base(got) != ".zordon" || zfs.Missing(filepath.Join(filepath.Dir(got), "go.mod")) {
		t.Errorf("Home = %q, want <repo>/.zordon", got)
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
