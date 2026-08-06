// Package example_test runs the mcp_http example as a conformance test: it
// brings the example up, then drives `zordon mcp --transport=http` with a real
// MCP client over the streamable-HTTP transport and asserts the contracts the
// Alphasfile claims —
//
//   - a client holding nothing but a URL gets the same tool set as a stdio
//     client: every zordon command plus every provision in the resolved chain;
//   - typed provision arguments survive the HTTP hop and reach the shell;
//   - a failed invoke over HTTP is reported WITHOUT tearing alpha down, the
//     same guarantee the stdio server makes.
package example_test

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/piotrkowalczuk/zordon/internal/zenv"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestExample_mcpHTTP(t *testing.T) {
	exampleDir := thisDir()
	p := zordontest.NewProject(t, zordontest.WithExistingRoot(exampleDir))
	p.Start(t).OK()

	seedPath := p.Get(t, "service.go.app.vars.seed").String()
	port := p.Get(t, "service.go.app.vars.port").String()
	if seedPath == "" || port == "" {
		t.Fatalf("vars resolved empty: seed=%q port=%q", seedPath, port)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Port 0: the server binds an ephemeral port and announces the resolved
	// URL, which is the only thing a remote client needs.
	url, stderr := startMCPHTTP(t, p, "127.0.0.1:0")

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "zordon-test", Version: "0.0.1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect mcp over http: %v\nstderr:\n%s", err, stderr())
	}
	defer cs.Close()

	// (0) The server advertises the same instructions it does over stdio.
	if init := cs.InitializeResult(); init == nil || !strings.Contains(init.Instructions, "Alphasfile") {
		t.Errorf("server instructions missing or unscoped: %+v", init)
	}

	// (1) tools/list over HTTP: every command (except `mcp` itself) and every
	// provision — the transport must not change the surface.
	tools := listTools(ctx, t, cs)
	for _, want := range []string{"start", "status", "stop", "get", "plan", "workspace", "sudo", "clean"} {
		if tools[want] == nil {
			t.Errorf("tools/list missing command tool %q\n%s", want, stderr())
		}
	}
	if tools["mcp"] != nil {
		t.Error("tools/list must not expose the `mcp` command as a tool")
	}
	for _, want := range []string{"provision__go_app__seed-data", "provision__go_app__boom"} {
		if tools[want] == nil {
			t.Errorf("tools/list missing provision tool %q\n%s", want, stderr())
		}
	}

	// (2) A command tool's output comes back in the result, and the resolved
	// value matches what the CLI reports for the same expression.
	got := callTool(ctx, t, cs, "get", map[string]any{"args": []string{"service.go.app.vars.port"}})
	if got.IsError || !strings.Contains(resultText(got), port) {
		t.Errorf("get tool output = %q; want it to contain port %q", resultText(got), port)
	}

	// (3) A declared, typed argument survives the HTTP hop and reaches the shell.
	seed := callTool(ctx, t, cs, "provision__go_app__seed-data", map[string]any{"key": "over-http"})
	if seed.IsError {
		t.Errorf("seed-data invoke errored:\n%s", resultText(seed))
	}
	if val := strings.TrimSpace(readFile(t, seedPath)); val != "over-http" {
		t.Errorf("seed file = %q; want %q (argument must reach the provision)", val, "over-http")
	}

	// (4) A FAILING invoke is reported as an error result and alpha survives —
	// the failfast=false guarantee, now over HTTP.
	if boom := callTool(ctx, t, cs, "provision__go_app__boom", nil); !boom.IsError {
		t.Errorf("boom invoke should be an error result; got:\n%s", resultText(boom))
	}
	if st := callTool(ctx, t, cs, "status", nil); st.IsError || !strings.Contains(resultText(st), "app") {
		t.Errorf("alpha not healthy after a FAILING invoke (failfast leaked into invoke?):\n%s", resultText(st))
	}

	// (5) The handler is mounted at /mcp only; anything else is a 404, so a
	// misconfigured client URL fails loudly instead of hanging.
	assertNotFound(ctx, t, strings.TrimSuffix(url, "/mcp")+"/")
}

// startMCPHTTP spawns `zordon mcp --transport=http` against the project (same
// ZORDON_HOME and cwd as the running alpha) and returns the URL it announced
// plus a getter for its stderr. Learning the URL from the log is what makes an
// ephemeral port usable, and it pins the announcement as a contract.
func startMCPHTTP(t *testing.T, p *zordontest.Project, listen string) (string, func() string) {
	t.Helper()
	cmd := exec.Command(zordonBin(t), "mcp", "--transport=http", "--listen", listen)
	cmd.Dir = p.Dir()
	cmd.Env = mcpEnv(p.Home())
	errBuf := &syncBuffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start zordon mcp --transport=http: %v", err)
	}

	// One owner for Wait; closing exited publishes waitErr to every reader.
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()

	t.Cleanup(func() {
		// SIGTERM first so the server takes its graceful-shutdown path; fall
		// back to a kill if it does not go.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if m := listenURLRe.FindString(errBuf.String()); m != "" {
			return m, errBuf.String
		}
		// A server that died (stale binary on $PATH, port taken, bad flag) has
		// already said why on stderr — report that instead of the deadline.
		select {
		case <-exited:
			t.Fatalf("zordon mcp exited before announcing its URL: %v\nstderr:\n%s", waitErr, errBuf.String())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("zordon mcp never announced its URL within 30s\nstderr:\n%s", errBuf.String())
	return "", errBuf.String
}

// listenURLRe matches the address line serveMCPHTTP logs once bound.
var listenURLRe = regexp.MustCompile(`http://[^\s]+/mcp`)

// syncBuffer collects the server's stderr while the test polls it for the
// announced URL, so the writing and reading goroutines do not race.
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

func listTools(ctx context.Context, t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}
	return byName
}

// callTool calls a tool and fails only on a protocol error; a tool-level
// failure (IsError) is a normal result the caller asserts on.
func callTool(ctx context.Context, t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	return res
}

func assertNotFound(ctx context.Context, t *testing.T, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET %s = %d; want 404 (the handler is mounted at /mcp only)", url, res.StatusCode)
	}
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// mcpEnv is the host env with ZORDON_HOME forced to the project's home so the
// MCP server resolves the same federation chain and alpha socket as `start`.
func mcpEnv(home string) []string {
	host := zenv.Environ()
	out := make([]string, 0, len(host)+1)
	for _, kv := range host {
		if strings.HasPrefix(kv, "ZORDON_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "ZORDON_HOME="+home)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := zfs.Read(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func thisDir() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Dir(here)
}

func zordonBin(t *testing.T) string {
	t.Helper()
	if v, ok := zenv.Lookup("ZORDON_BIN"); ok && v != "" {
		return v
	}
	bin, err := exec.LookPath("zordon")
	if err != nil {
		t.Fatalf("zordon binary not found: set $ZORDON_BIN or `go install ./cmd/zordon`")
	}
	return bin
}
