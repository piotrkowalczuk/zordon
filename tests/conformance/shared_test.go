package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Toolchain-agnostic helpers shared across the tagged conformance
// buckets (conformance_go / _rust / _node / _pkg). This file carries no
// build tag on purpose: an untagged _test.go compiles into every
// `-tags conformance_*` build and the plain untagged run, so a single
// definition serves all legs.

func mustStart(t *testing.T, p *zordontest.Project) {
	t.Helper()
	p.Start(t).OK()
}

// echoResponse is the JSON shape golden/<lang>/echo's GET / returns —
// every language fixture emits the identical wire shape. Tests that
// only care about reachability use the per-language mustGet*Echo; tests
// that inspect specific fields use mustDecodeEcho.
type echoResponse struct {
	Env            map[string]string `json:"env"`
	Cwd            string            `json:"cwd"`
	Argv           []string          `json:"argv"`
	RuntimeVersion string            `json:"runtime_version"`
}

func mustDecodeEcho(t testing.TB, port int) echoResponse {
	t.Helper()
	// Retry: readiness having passed means the service served alpha's probe,
	// but the test connects from a different process a beat later — on a
	// loaded CI runner the accept loop can briefly refuse, or a slow runtime
	// (rust cold-build) can lag the first external request. A genuinely dead
	// service stays refused for the whole window, so this masks the race, not
	// real failures.

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	for {
		echo, err := tryDecodeEcho(ctx, url)
		if err == nil {
			return echo
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for echo to start: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func tryDecodeEcho(ctx context.Context, url string) (echoResponse, error) {
	var echo echoResponse
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return echo, err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return echo, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return echo, err
	}
	if res.StatusCode != http.StatusOK {
		return echo, fmt.Errorf("status %d: %s", res.StatusCode, body)
	}
	if err := json.NewDecoder(strings.NewReader(string(body))).Decode(&echo); err != nil {
		return echo, fmt.Errorf("decode echo: %w (body: %s)", err, body)
	}

	return echo, nil
}

// readFile is a thin testing.T-aware wrapper that returns content
// as string for tests asserting on generated-file bodies.
func readFile(t *testing.T, path string) (string, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// assertCwdEndsWithExe is the exe-anchor invariant: build+runtime cwd is
// `<src.path>/<exe>`, not just `<src.path>`.
func assertCwdEndsWithExe(t *testing.T, cwd string, parts ...string) {
	t.Helper()
	want := filepath.Join(parts...)
	if !strings.HasSuffix(cwd, want) {
		t.Errorf("service cwd = %q; want a path ending in %q (build/runtime should anchor on `<src.path>/<exe>`, not just `<src.path>`)",
			cwd, want)
	}
}

// mustWorkspaceStart runs `zordon workspace create <wt> <svc>` and then
// `zordon start` from inside the workspace dir. The two halves are split
// so a failure in `workspace create` (git-init missing, bad checkout)
// surfaces with its own error before alpha is involved. Returns the
// port the service bound: each fixture uses net::pickport(), so the
// value is read back from the running workspace alpha (get must run from
// the workspace dir, where that alpha lives) rather than hardcoded.
func mustWorkspaceStart(t *testing.T, p *zordontest.Project, wt, tc, svc string) int {
	t.Helper()
	res := p.Zordon("workspace", "create", wt, svc).WithTimeout(5 * time.Minute).Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon workspace create %s %s: exit %d\nstdout: %s\nstderr: %s",
			wt, svc, res.ExitCode, res.Stdout, res.Stderr)
	}
	wtRel := filepath.Join("workspaces", wt)
	// Start tears the workspace alpha down from wtRel on cleanup (the
	// project-root stop can't reach it), so no manual Stop here.
	p.Start(t, zordontest.StartIn(wtRel)).OK()
	return p.GetIn(t, wtRel, fmt.Sprintf("service.%s.%s.vars.port", tc, svc)).Int()
}

// mustCwdInWorkspace pins the observable shape of a workspace run: the
// spawned service's cwd is its per-workspace checkout dir, not the
// project root and not the main workspace's checkout.
func mustCwdInWorkspace(t *testing.T, cwd string, p *zordontest.Project, wt, svc string) {
	t.Helper()
	want := filepath.Join(p.Dir(), "workspaces", wt, "src", svc)
	// macOS sometimes returns /private/var/... for /var/... — match by
	// suffix so the assertion isn't flaky on the symlinked tmpdir.
	if !strings.HasSuffix(cwd, filepath.Join("workspaces", wt, "src", svc)) {
		t.Errorf("service cwd = %q; want a path ending with %q (the per-workspace checkout, not the anchor)",
			cwd, want)
	}
}

// mustZordonOK runs a zordon subcommand and fails the test unless it exits 0,
// surfacing stdout+stderr so a non-zero exit reads with its cause.
func mustZordonOK(t *testing.T, p *zordontest.Project, args ...string) {
	t.Helper()
	res := p.Zordon(args...).WithTimeout(2 * time.Minute).Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon %s: exit %d\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), res.ExitCode, res.Stdout, res.Stderr)
	}
}
