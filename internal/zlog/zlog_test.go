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
