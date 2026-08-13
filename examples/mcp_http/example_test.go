// Package example_test runs the mcp_http example against a live stack: it
// brings the example up, then drives `zordon mcp --transport=http` with a real
// MCP client and asserts the contracts the Alphasfile claims —
//
//   - a client holding nothing but a URL gets the same tool set as a stdio
//     client: every zordon command plus every provision in the resolved chain;
//   - typed provision arguments survive the HTTP hop and reach the shell;
//   - a failed invoke over HTTP is reported WITHOUT tearing alpha down, the
//     same guarantee the stdio server makes.
//
// The transport's own contract — the address it announces, the /mcp mount and
// the Host guard — needs no stack and lives in tests/conformance/mcp_http_test.go.
package example_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestExample_mcpHTTP(t *testing.T) {
	p := zordontest.NewProject(t, zordontest.WithCallerRoot())
	p.Start(t).OK()

	seedPath := p.Get(t, "service.go.app.vars.seed").String()
	port := p.Get(t, "service.go.app.vars.port").String()
	if seedPath == "" || port == "" {
		t.Fatalf("vars resolved empty: seed=%q port=%q", seedPath, port)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	// The harness binds an ephemeral port and hands back the announced URL —
	// the only thing a remote client ever needs.
	srv := p.MCPHTTP(t)

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "zordon-test", Version: "0.0.1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL()}, nil)
	if err != nil {
		t.Fatalf("connect mcp over http: %v\nstderr:\n%s", err, srv.Stderr())
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
			t.Errorf("tools/list missing command tool %q\n%s", want, srv.Stderr())
		}
	}
	if tools["mcp"] != nil {
		t.Error("tools/list must not expose the `mcp` command as a tool")
	}
	for _, want := range []string{"provision__go_app__seed-data", "provision__go_app__boom"} {
		if tools[want] == nil {
			t.Errorf("tools/list missing provision tool %q\n%s", want, srv.Stderr())
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

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := zfs.Read(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
