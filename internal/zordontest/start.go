package zordontest

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"
)

// StartOption configures a Start invocation.
type StartOption func(*startOpts)

type startOpts struct {
	dir     string        // cwd relative to project root (e.g. a workspace subdir)
	bringup time.Duration // zordon's own --timeout: the service bringup budget
	env     map[string]string
}

// StartIn runs `zordon start` from relPath (relative to the project
// root) instead of the root — drive a workspace subdir the way a
// developer `cd`s into it.
func StartIn(relPath string) StartOption {
	return func(o *startOpts) { o.dir = relPath }
}

// StartTimeout overrides the bringup budget. It feeds both `--timeout`
// and — one minute higher, so a slow-but-valid start is never falsely
// killed — the harness process kill. The process kill is what actually
// bounds a bringup: `--timeout` covers only alpha's spawn -> READY ->
// listen handshake, because pushConfigure clears the socket deadline
// once configure is on the wire. Default is 15m — enough for a cold
// toolchain install on first run.
func StartTimeout(bringup time.Duration) StartOption {
	return func(o *startOpts) { o.bringup = bringup }
}

// StartEnv sets KEY=VALUE for this one invocation (overrides inherited).
func StartEnv(key, value string) StartOption {
	return func(o *startOpts) {
		if o.env == nil {
			o.env = map[string]string{}
		}
		o.env[key] = value
	}
}

// Start runs `zordon start`, always wiring `--alpha-log` so AlphaLog()
// can read the bringup log back, and returns the outcome to assert on:
//
//	p.Start(t).OK()                                  // must come up
//	p.Start(t).Failed().OutputContains("toolchain")  // must reject, with a reason
//
// Start does not assert by itself — the expectation lives at the call
// site, so a test that means "this must fail" reads that way. Pass
// testing.TB explicitly so the helper can be driven from a subtest's t.
func (p *Project) Start(t testing.TB, opts ...StartOption) *StartOutcome {
	t.Helper()
	o := startOpts{bringup: 15 * time.Minute}
	for _, fn := range opts {
		fn(&o)
	}

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Start failed, dumping alpha log %s:", p.AlphaLogPath())
			if f, err := os.Open(p.AlphaLogPath()); err != nil {
				// Never t.Fatal in a Cleanup: FailNow's runtime.Goexit
				// aborts the rest of the cleanup chain.
				t.Logf("(open alpha log: %v)", err)
			} else {
				s := bufio.NewScanner(f)
				for s.Scan() {
					t.Log(s.Text())
				}
				if err := f.Close(); err != nil {
					t.Error(err)
				}
			}
		}
		// Stop the alpha from wherever Start launched it (o.dir): a
		// workspace-level alpha (StartIn) isn't reachable by the project-root
		// cleanup, so without this it leaks. Symmetric with Start, so callers
		// don't hand-roll a Stop. Best-effort, no t.Fatal (we're in Cleanup).
		_ = p.StopFrom(o.dir)
	})
	cmd := p.Zordon("start",
		"--timeout", o.bringup.String(),
		"--alpha-log", p.AlphaLogPath(),
	).WithTimeout(o.bringup + time.Minute)
	if o.dir != "" {
		cmd = cmd.WithDir(o.dir)
	}
	for k, v := range o.env {
		cmd = cmd.WithEnv(k, v)
	}

	what := "zordon start"
	if o.dir != "" {
		what += " (in " + o.dir + ")"
	}
	return &StartOutcome{t: t, res: cmd.Run(t), what: what}
}

// StartOutcome is one `zordon start` result with fluent assertions, so
// tests pin the expectation instead of poking at ExitCode/Stdout/Stderr
// by hand.
type StartOutcome struct {
	t    testing.TB
	res  ZordonResult
	what string
}

// OK fails the test unless the start exited 0 (services came up).
func (o *StartOutcome) OK() *StartOutcome {
	o.t.Helper()
	if o.res.ExitCode != 0 {
		o.t.Fatalf("%s: exit %d, want success\n%s", o.what, o.res.ExitCode, o.dump())
	}
	return o
}

// Failed fails the test unless the start exited non-zero. Use it for
// "must be rejected" scenarios (parse errors, failfast provisions), then
// chain OutputContains to pin the reason.
func (o *StartOutcome) Failed() *StartOutcome {
	o.t.Helper()
	if o.res.ExitCode == 0 {
		o.t.Fatalf("%s: exit 0, want failure\n%s", o.what, o.dump())
	}
	return o
}

// OutputContains reports (non-fatally, so every gap is surfaced at once)
// any want that is absent from the combined stdout+stderr — the
// substrings a diagnostic must mention.
func (o *StartOutcome) OutputContains(want ...string) *StartOutcome {
	o.t.Helper()
	for _, w := range missingSubstrings(o.Output(), want) {
		o.t.Errorf("%s: output missing %q\n%s", o.what, w, o.dump())
	}
	return o
}

// ExitCode is the raw exit code, for the rare assertion the fluent
// helpers don't cover.
func (o *StartOutcome) ExitCode() int { return o.res.ExitCode }

// Output is the combined stdout+stderr (the order zordon interleaves
// diagnostics doesn't matter for substring checks).
func (o *StartOutcome) Output() string { return o.res.Stdout + o.res.Stderr }

func (o *StartOutcome) dump() string {
	return "--- stdout ---\n" + o.res.Stdout + "\n--- stderr ---\n" + o.res.Stderr
}

// missingSubstrings returns the wants not present in haystack, preserving
// order. Pure so it can be unit-tested without a real zordon run.
func missingSubstrings(haystack string, wants []string) []string {
	var missing []string
	for _, w := range wants {
		if !strings.Contains(haystack, w) {
			missing = append(missing, w)
		}
	}
	return missing
}
