// Shared log-scanning helpers for conformance tests that read alpha.log
// to identify alpha-process PIDs and wait for specific markers.
package conformance_test

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// reAlphaStartAll matches every "starting pid=N" line in an alpha.log.
// alpha logs this once at process startup (see runAlpha), so each match
// corresponds to one alpha lifetime in a shared log file.
var reAlphaStartAll = regexp.MustCompile(`starting pid=(\d+)`)

// lastAlphaPIDFromLog returns the PID from the LAST "starting pid=N"
// line in logPath — useful when the log accumulates many alpha
// lifetimes (multi-iter tests) and the caller wants the most recent.
// Returns 0 if no match. Fails the test if logPath can't be read.
func lastAlphaPIDFromLog(t *testing.T, logPath string) int {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read %s: %v", logPath, err)
	}
	matches := reAlphaStartAll.FindAllSubmatch(b, -1)
	if len(matches) == 0 {
		return 0
	}
	pid, _ := strconv.Atoi(string(matches[len(matches)-1][1]))
	return pid
}

// waitForLogContent blocks until `substr` appears anywhere in logPath,
// or `within` elapses. Use when there's only one zordon invocation
// running (scan from byte 0 is safe). Tests with multi-iter logs that
// reuse a single alpha.log should use an offset-aware variant instead.
func waitForLogContent(t *testing.T, logPath, substr string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(logPath)
		if err == nil && len(b) > 0 {
			for i := 0; i+len(substr) <= len(b); i++ {
				if string(b[i:i+len(substr)]) == substr {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("log content %q did not appear within %s; log %s", substr, within, logPath)
}
