// Package example_test runs the go_delve example as a conformance
// test: what the README claims, the test asserts. The example's
// Alphasfile + source tree are the fixtures; the test only drives
// zordon and reads back observable outputs.
package example_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// TestExample_go_delve asserts the `debugger { enabled = true }` macro:
//
//  1. The inferior comes up under dlv and still serves HTTP on its
//     own picked port (proves runtime wrapping didn't break readiness).
//  2. dlv's headless DAP listener answers on the resolved debugger
//     port (proves the wrap actually started a debugger, not just
//     prefixed argv with /usr/bin/false).
//
// We run zordon IN PLACE via WithCallerRoot rather than copying to a
// tmpdir — the example uses `src = "../.."` (the surrounding repo)
// and the test wants to exercise the real wiring.
func TestExample_go_delve(t *testing.T) {
	p := zordontest.NewProject(t, zordontest.WithCallerRoot())

	// Cold-cache budget: cargo install mise (first run) + mise install
	// go@1.25.6 + first install of dlv + mcp-dap-server + first compile.
	// Slightly higher than the plain go example because of the extra
	// `go install` for the macro-injected tools.
	p.Start(t).OK()

	// HTTP side: the inferior is actually serving. If the debugger
	// macro had silently broken the wrap, readiness would have failed
	// upstream and `zordon start` wouldn't have returned 0 — but assert
	// the body anyway so a future regression that fakes readiness still
	// gets caught here.
	httpPort := p.Get(t, "service.go.app.vars.port").Int()
	if httpPort <= 0 {
		t.Fatalf("vars.port = %d; want a valid TCP port", httpPort)
	}
	body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", httpPort))
	if !strings.Contains(body, "go-delve-example ok") {
		t.Errorf("HTTP body = %q; want it to contain %q", body, "go-delve-example ok")
	}

	// DAP side: the debugger.port value was resolved by the macro
	// (default 2345, or $DLV_PORT). Probe it with a single DAP
	// Initialize request — that proves dlv is speaking DAP, not just
	// that *something* accepted TCP.
	dapPort := p.Get(t, "service.go.app.debugger.port").Int()
	if dapPort <= 0 {
		t.Fatalf("debugger.port = %d; want a valid TCP port", dapPort)
	}
	if got := p.Get(t, "service.go.app.agent.mcp.debug.address").String(); got != fmt.Sprintf("127.0.0.1:%d", dapPort) {
		t.Errorf("agent.mcp.debug.address = %q; want %q", got, fmt.Sprintf("127.0.0.1:%d", dapPort))
	}
	if err := dapInitialize(fmt.Sprintf("127.0.0.1:%d", dapPort)); err != nil {
		t.Errorf("dlv DAP listener not responding on port %d: %v", dapPort, err)
	}
}

// dapInitialize speaks the Debug Adapter Protocol's minimal handshake:
// one `initialize` request, one `initialized` event or response in
// reply. Each DAP message is a Content-Length-prefixed JSON object,
// like HTTP — so we send the header + body and read back enough to
// confirm the server is alive.
//
// We don't fully parse the response. dlv either replies with a
// well-formed Content-Length frame (success) or closes/errors
// (failure); reading any bytes from the socket within the timeout is
// sufficient evidence that a real DAP server is on the other end.
func dapInitialize(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	body := `{"seq":1,"type":"request","command":"initialize","arguments":{"adapterID":"go","clientID":"zordon-test","linesStartAt1":true,"columnsStartAt1":true,"pathFormat":"path"}}`
	msg := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	if _, err := conn.Write([]byte(msg)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	// Read the response header — DAP always replies "Content-Length: …\r\n\r\n…".
	// 18 bytes covers the literal prefix "Content-Length: 1"; anything
	// shorter means the server hung up without replying.
	buf := make([]byte, 64)
	n, err := io.ReadAtLeast(conn, buf, 18)
	if err != nil {
		return fmt.Errorf("read: %w (got %d bytes: %q)", err, n, buf[:n])
	}
	if !strings.HasPrefix(string(buf[:n]), "Content-Length:") {
		return fmt.Errorf("unexpected reply, want DAP frame header: %q", buf[:n])
	}
	return nil
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(b)
}
