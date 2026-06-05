// Package example_test runs the mcp example as a conformance test: it brings
// the example up, then drives `zordon mcp` with a real MCP client over stdio
// and asserts the two contracts the README claims —
//
//   - discoverability: tools/list exposes every zordon command plus every
//     provision in the resolved chain;
//   - provisions as on-demand scripts WITHOUT killing alpha: invoking a
//     provision runs it inside the live alpha; a failed (or shut-down) invoke
//     is reported but alpha keeps running.
package example_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestExample_mcp(t *testing.T) {
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
	cs, stderr := connectMCP(ctx, t, p)
	defer cs.Close()

	// (1,2) tools/list: every command (except `mcp` itself) and every provision.
	tools := listTools(ctx, t, cs)
	for _, want := range []string{"start", "status", "stop", "get", "plan", "worktree", "sudo"} {
		if tools[want] == nil {
			t.Errorf("tools/list missing command tool %q\n%s", want, stderr())
		}
	}
	if tools["mcp"] != nil {
		t.Error("tools/list must not expose the `mcp` command as a tool")
	}
	for _, want := range []string{
		"provision__go_app__seed-data",
		"provision__go_app__init-state",
		"provision__go_app__boom",
		"provision__go_app__slow",
	} {
		if tools[want] == nil {
			t.Errorf("tools/list missing provision tool %q\n%s", want, stderr())
		}
	}
	// The Alphasfile's `description` reaches the MCP tool description, and its
	// declared `argument "key"` becomes a required, typed input-schema field.
	seedTool := tools["provision__go_app__seed-data"]
	if seedTool == nil || !strings.Contains(seedTool.Description, "Write the seed marker file") {
		t.Errorf("seed-data tool description missing the authored text:\n%+v", seedTool)
	} else if !schemaHasRequiredField(seedTool.InputSchema, "key") {
		t.Errorf("seed-data input schema missing required `key` field: %+v", seedTool.InputSchema)
	}

	// (3) Invoke with the declared argument: the value reaches the shell.
	seed := callTool(ctx, t, cs, "provision__go_app__seed-data", map[string]any{"key": "abc"})
	if seed.IsError {
		t.Errorf("seed-data invoke errored:\n%s", resultText(seed))
	}
	if got := strings.TrimSpace(readFile(t, seedPath)); got != "abc" {
		t.Errorf("seed file = %q; want %q (argument must reach the provision)", got, "abc")
	}

	// (4) Alpha survives a successful invoke.
	if st := callTool(ctx, t, cs, "status", nil); st.IsError || !strings.Contains(resultText(st), "app") {
		t.Errorf("alpha not healthy after a successful invoke:\n%s", resultText(st))
	}

	// (5) Alpha survives a FAILING invoke — the failfast=false guarantee.
	if boom := callTool(ctx, t, cs, "provision__go_app__boom", nil); !boom.IsError {
		t.Errorf("boom invoke should be an error result; got:\n%s", resultText(boom))
	}
	if st := callTool(ctx, t, cs, "status", nil); st.IsError || !strings.Contains(resultText(st), "app") {
		t.Errorf("alpha not healthy after a FAILING invoke (failfast leaked into invoke?):\n%s", resultText(st))
	}

	// (6) Re-invoking an auto-run, idempotent provision succeeds (check skips cmd).
	if init := callTool(ctx, t, cs, "provision__go_app__init-state", nil); init.IsError {
		t.Errorf("init-state re-invoke errored:\n%s", resultText(init))
	}

	// (10) Command-tool output is captured into the result, not leaked onto the
	// JSON-RPC stdout channel — and the channel stays usable afterwards.
	got := callTool(ctx, t, cs, "get", map[string]any{"args": []string{"service.go.app.vars.port"}})
	if got.IsError || !strings.Contains(resultText(got), port) {
		t.Errorf("get tool output = %q; want it to contain port %q", resultText(got), port)
	}
	if after := listTools(ctx, t, cs); after["status"] == nil {
		t.Error("JSON-RPC channel unusable after a command tool call (stdout corruption?)")
	}

	// (11) A `zordon stop` mid-invoke aborts the in-flight provision cleanly.
	// AssertNoLeftovers (harness cleanup) backs the "no orphaned process" claim.
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "provision__go_app__slow"})
		if err != nil {
			done <- nil
			return
		}
		done <- r
	}()
	time.Sleep(1500 * time.Millisecond) // let the slow provision's shell start
	p.Zordon("stop").Run(t)
	select {
	case r := <-done:
		if r != nil && !r.IsError {
			t.Errorf("slow invoke should not have succeeded through a shutdown:\n%s", resultText(r))
		}
	case <-time.After(20 * time.Second):
		t.Error("slow invoke never returned after `zordon stop` (orphaned/blocked)")
	}

	// (9) With alpha down, a provision tool reports a clear, actionable error.
	gone := callTool(ctx, t, cs, "provision__go_app__seed-data", map[string]any{"key": "zzz"})
	if !gone.IsError {
		t.Errorf("invoke with alpha down should error; got:\n%s", resultText(gone))
	}
	if !strings.Contains(resultText(gone), "not running") {
		t.Errorf("invoke with alpha down: want a 'not running' hint; got:\n%s", resultText(gone))
	}
}

// connectMCP spawns `zordon mcp` against the project (same ZORDON_HOME and cwd
// as the running alpha) and returns a connected client session plus a getter
// for the server's stderr (diagnostics, surfaced on failure).
func connectMCP(ctx context.Context, t *testing.T, p *zordontest.Project) (*mcp.ClientSession, func() string) {
	t.Helper()
	cmd := exec.Command(zordonBin(t), "mcp")
	cmd.Dir = p.Dir()
	cmd.Env = mcpEnv(p.Home())
	var errBuf strings.Builder
	cmd.Stderr = &errBuf

	client := mcp.NewClient(&mcp.Implementation{Name: "zordon-test", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect mcp: %v\nstderr:\n%s", err, errBuf.String())
	}
	return cs, errBuf.String
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

// schemaHasRequiredField reports whether a JSON-schema (as the client decodes
// it, a map[string]any) declares `field` as a required property.
func schemaHasRequiredField(schema any, field string) bool {
	m, ok := schema.(map[string]any)
	if !ok {
		return false
	}
	if props, ok := m["properties"].(map[string]any); !ok || props[field] == nil {
		return false
	}
	req, _ := m["required"].([]any)
	for _, r := range req {
		if s, _ := r.(string); s == field {
			return true
		}
	}
	return false
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
	out := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ZORDON_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "ZORDON_HOME="+home)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
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
	if v := os.Getenv("ZORDON_BIN"); v != "" {
		return v
	}
	bin, err := exec.LookPath("zordon")
	if err != nil {
		t.Fatalf("zordon binary not found: set $ZORDON_BIN or `go install ./cmd/zordon`")
	}
	return bin
}
