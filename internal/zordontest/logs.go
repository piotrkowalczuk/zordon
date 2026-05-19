package zordontest

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// LogReader is a read-side view of a single log file alpha produced
// during a project's lifetime. Construct via Project.AlphaLog().
//
// The file is re-read on every method call — log files grow during
// a test (alpha keeps appending) and we want assertions to see the
// latest content, not a snapshot from the LogReader's construction
// time. The cost is negligible (logs are bytes, not gigabytes).
type LogReader struct {
	t    *testing.T
	path string
}

// AlphaLog returns a reader for this project's alpha log. Tests must
// have passed `--alpha-log p.AlphaLogPath()` to a previous
// `zordon start` invocation for the file to exist; reading an absent
// log fails the test (it's almost always a setup mistake).
func (p *Project) AlphaLog() *LogReader {
	return &LogReader{t: p.t, path: p.AlphaLogPath()}
}

// Contents returns the full file as a string. Fails the test if the
// file doesn't exist — the typical mistake (forgetting `--alpha-log`)
// would otherwise yield silent empty-string assertions.
func (r *LogReader) Contents() string {
	r.t.Helper()
	data, err := os.ReadFile(r.path)
	if err != nil {
		r.t.Fatalf("read alpha log %s: %v", r.path, err)
	}
	return string(data)
}

// Lines returns the log split on '\n', trailing empty line dropped.
// Useful when callers want to count entries or window the tail.
func (r *LogReader) Lines() []string {
	c := strings.TrimRight(r.Contents(), "\n")
	if c == "" {
		return nil
	}
	return strings.Split(c, "\n")
}

// Contains reports whether substr appears anywhere in the log.
// Fast path for "did alpha mention X at all" assertions.
func (r *LogReader) Contains(substr string) bool {
	return strings.Contains(r.Contents(), substr)
}

// Filter returns every line for which pred returns true. Tests use
// this when "contains X" isn't precise enough (e.g. "an INFO line
// from source 'app' mentioning Y").
func (r *LogReader) Filter(pred func(line string) bool) []string {
	var out []string
	for _, ln := range r.Lines() {
		if pred(ln) {
			out = append(out, ln)
		}
	}
	return out
}

// Match returns every line matching re. Same intent as Filter, but
// for the common regex case.
func (r *LogReader) Match(re *regexp.Regexp) []string {
	return r.Filter(re.MatchString)
}
