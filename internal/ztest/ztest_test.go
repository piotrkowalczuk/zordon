package ztest

import (
	"fmt"
	"testing"
)

func TestSkip_strictFails(t *testing.T) {
	t.Setenv(NoSkipEnv, "1")
	rec := &recorder{TB: t}
	func() {
		defer func() { _ = recover() }()
		Skip(rec, "missing %s", "cargo")
	}()
	if !rec.failed || rec.skipped {
		t.Fatalf("strict mode must fail, not skip: failed=%v skipped=%v", rec.failed, rec.skipped)
	}
	if want := "skip forbidden (ZORDON_NO_SKIP=1): missing cargo"; rec.msg != want {
		t.Errorf("message = %q, want %q", rec.msg, want)
	}
}

func TestSkip_localSkips(t *testing.T) {
	t.Setenv(NoSkipEnv, "")
	rec := &recorder{TB: t}
	func() {
		defer func() { _ = recover() }()
		Skip(rec, "missing %s", "cargo")
	}()
	if rec.failed || !rec.skipped || rec.msg != "missing cargo" {
		t.Fatalf("local mode must skip: failed=%v skipped=%v msg=%q", rec.failed, rec.skipped, rec.msg)
	}
}

// recorder captures Fatalf/Skip instead of ending the enclosing test; the
// panic stands in for runtime.Goexit so Skip returns control here.
type recorder struct {
	testing.TB
	failed, skipped bool
	msg             string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
	panic("fatal")
}

func (r *recorder) Skip(args ...any) {
	r.skipped = true
	if len(args) == 1 {
		r.msg, _ = args[0].(string)
	}
	panic("skip")
}
