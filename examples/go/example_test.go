// Package example_test runs the go example as a conformance test:
// what the README claims, the test asserts. The example's Alphasfile
// + source tree are the fixtures; the test only drives zordon and
// reads back observable outputs.
package example_test

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// TestExample_go is the Go-runnable replacement for the legacy
// `test.sh` in this dir. Claim under test:
//
//	A Go service built from src=../.. (i.e. the zordon repo itself)
//	comes up and serves HTTP, and the startup `fmt.Printf("up ...")`
//	reaches alpha's log (proving stdout isn't swallowed by user-
//	space buffering — the bug that motivated the per-toolchain TTY
//	default).
//
// The example's Alphasfile keeps `src = "../.."` so it depends on
// the surrounding repo layout — we run zordon IN PLACE via
// WithExistingRoot rather than copying to a tmpdir.
func TestExample_go(t *testing.T) {
	exampleDir := thisDir()
	p := zordontest.NewProject(t, zordontest.WithExistingRoot(exampleDir))

	// Cold-cache budget: cargo install mise (first run, ~800 crates)
	// + mise install go@1.25.6 + first compile of the example. ~10
	// minutes peak on a beefy laptop; CI numbers should land near
	// it. Subsequent runs reuse <repo>/.zordon and finish in seconds.
	p.Start(t).OK()

	// Port resolved from Alphasfile's `vars = { port = net::pickport() }`
	// via zordon's public expression evaluator — no ps-grep, no
	// log scraping.
	port := p.Get(t, "service.go.app.vars.port").Int()
	if port <= 0 {
		t.Fatalf("port = %d; want a valid TCP port", port)
	}

	body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", port))
	wantBody := "go-example ok"
	if !strings.Contains(body, wantBody) {
		t.Errorf("body = %q; want it to contain %q", body, wantBody)
	}

	// The "up <addr>" line from main.go's startup must reach alpha's
	// log: stdout written by the service shouldn't be eaten by
	// libc-level line buffering. Go writes through write(2) which
	// works under a pipe, so this is the easy case (ruby/python
	// would need PTY default).
	wantLog := fmt.Sprintf("up 127.0.0.1:%d", port)
	if !p.AlphaLog().Contains(wantLog) {
		t.Errorf("alpha log missing startup line %q", wantLog)
	}
}

// thisDir returns the absolute directory of the file invoking it —
// i.e. examples/go/. Used so the test can locate its own example
// tree without depending on the CWD `go test` happens to run from
// (`go test ./examples/go/...` runs with CWD=examples/go/, but
// `go test ./...` from repo root doesn't — runtime.Caller decouples).
func thisDir() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Dir(here)
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body)
}
