package zlog

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A forwarded line that opens a colour without closing it (its reset may have
// been dropped by a log filter) must not bleed past its own line: colour mode
// closes every line with a reset.
func TestLogger_Service_colorResetsEachLine(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{w: &buf, color: true, startedAt: time.Now()}

	l.Service("app", "stdout", "\x1b[34mblue with no reset")

	out := buf.String()
	if !strings.HasSuffix(out, "\x1b[34mblue with no reset"+ansiReset+"\n") {
		t.Fatalf("colored line must end with a reset before the newline; got %q", out)
	}
}

// Non-colour mode (files, agent consumers, non-TTY) must stay byte-for-byte
// ANSI-free — this is what golden logs and AlphaLog assertions read.
func TestLogger_Service_noColorNoAnsi(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{w: &buf, color: false, startedAt: time.Now()}

	l.Service("app", "stdout", "plain line")

	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("non-color mode must emit no ANSI; got %q", out)
	}
	if !strings.HasSuffix(out, "- plain line\n") {
		t.Fatalf("unexpected line shape: %q", out)
	}
}

func TestLogger_WithPID(t *testing.T) {
	var humanBuf bytes.Buffer
	l := New(&humanBuf, false).WithPID(85531)
	l.Info("alpha", "removed file %s", ".env")
	l.Service("svc1", "stdout", "hello from service")
	l.Info("payments-service-long", "prepare")
	got := humanBuf.String()
	if !strings.Contains(got, "alpha[85531]    [INFO ] - removed file .env\n") {
		t.Fatalf("human mode WithPID output = %q; want alpha[85531] column", got)
	}
	if !strings.Contains(got, "svc1            [LOG  ] - hello from service\n") {
		t.Fatalf("human mode WithPID must NOT tag forwarded service output with alpha PID; got %q", got)
	}
	if strings.Contains(got, "payments-service-long[") {
		t.Fatalf("human mode WithPID must NOT tag service source names; got %q", got)
	}

	var agentBuf bytes.Buffer
	al := New(&agentBuf, true).WithPID(63873)
	al.Warn("registry", "orphan reaper")
	al.Service("svc1", "stdout", "line")
	agentGot := agentBuf.String()
	if !strings.Contains(agentGot, " registry[63873] WARN orphan reaper\n") {
		t.Fatalf("agent mode WithPID output = %q; want registry[63873] column", agentGot)
	}
	if !strings.Contains(agentGot, " svc1 STDOUT line\n") {
		t.Fatalf("agent mode WithPID must NOT tag forwarded service output; got %q", agentGot)
	}
}
