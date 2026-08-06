package zordontest

import (
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// MCPServer is a running `zordon mcp --transport=http` for a project. It is a
// long-lived process rather than a one-shot invocation, so it does not fit the
// ZordonCmd builder: the test needs the address it bound while it keeps
// serving.
type MCPServer struct {
	url    string
	stderr func() string
}

// URL is the endpoint the server announced, e.g. http://127.0.0.1:54321/mcp.
// Point an MCP client at it, or POST to it directly.
func (s *MCPServer) URL() string { return s.url }

// Stderr returns everything the server has logged so far, for failure output.
func (s *MCPServer) Stderr() string { return s.stderr() }

// MCPHTTP starts an HTTP MCP server for the project on an ephemeral port and
// waits until it announces its address. extraArgs are appended to
// `mcp --transport=http --listen 127.0.0.1:0`, e.g. "--allow-host", "…".
//
// The server is stopped at test cleanup. Because it binds port 0, concurrent
// tests never collide.
func (p *Project) MCPHTTP(t *testing.T, extraArgs ...string) *MCPServer {
	t.Helper()

	args := append([]string{"mcp", "--transport=http", "--listen", "127.0.0.1:0"}, extraArgs...)
	cmd := exec.Command(p.binZ, args...)
	cmd.Dir = p.root
	cmd.Env = p.env()
	logs := &syncBuffer{}
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start zordon %s: %v", strings.Join(args, " "), err)
	}

	// One owner for Wait; closing exited publishes waitErr to every reader.
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()

	t.Cleanup(func() {
		// SIGTERM is the documented way to stop it, and it is supposed to drain
		// and exit rather than be killed — so needing the fallback is itself a
		// failure, reported here so every caller gets that check for free.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			t.Errorf("zordon mcp ignored SIGTERM for 10s and had to be killed\nstderr:\n%s", logs.String())
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if m := mcpURLRe.FindString(logs.String()); m != "" {
			return &MCPServer{url: m, stderr: logs.String}
		}
		// A server that died (port taken, bad flag, binary too old) has already
		// said why — report that instead of waiting out the deadline.
		select {
		case <-exited:
			hint := ""
			if strings.Contains(logs.String(), "unknown flag") {
				hint = "\n\nthat binary predates these flags — run `make build`, or point $ZORDON_BIN at a current build."
			}
			t.Fatalf("zordon mcp (%s) exited before announcing its URL: %v%s\nstderr:\n%s",
				cmd.Path, waitErr, hint, logs.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("zordon mcp never announced its URL within 30s\nstderr:\n%s", logs.String())
	return nil
}

// PostMCP sends a well-formed MCP initialize to url and returns the status
// code, so a rejection can only come from the path or the Host header, never
// from a malformed body. An empty host leaves Go's default, which is the
// dialled (loopback) address.
func PostMCP(t *testing.T, url, host string) int {
	t.Helper()
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"zordontest","version":"1"}}}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if host != "" {
		// Must be Request.Host: net/http ignores a "Host" entry in Header.
		req.Host = host
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

// mcpURLRe matches the address line the server logs once bound — the only way
// to learn an ephemeral port.
var mcpURLRe = regexp.MustCompile(`http://[^\s]+/mcp`)

// syncBuffer collects a server's stderr while the test polls it, so the
// writing and reading goroutines do not race.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
