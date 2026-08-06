package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/control"
	"github.com/piotrkowalczuk/zordon/internal/mcpserver"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// commandToolInput is the uniform input for every command tool. Per-command
// flags are documented in the tool description (and `["-h"]` returns full
// help); the agent passes them through here. A structured `args` array keeps
// the schema robust across ff's flag types instead of mirroring each flag.
type commandToolInput struct {
	Args []string `json:"args,omitempty" jsonschema:"command-line flags and positional arguments for the subcommand, e.g. [\"--failfast=false\",\"web\"]; pass [\"-h\"] for full help"`
}

// runMCP serves an MCP server over the configured transport — stdio for a
// client that launches zordon as a subprocess, HTTP for one that cannot. The
// tool set is identical either way. Two tool families:
//
//   - command tools — one per zordon subcommand (except `mcp` itself),
//     generated from the same ff tree the CLI uses, so the MCP surface can
//     never drift from the CLI. Re-run in-process against capture buffers; the
//     output is returned as the tool result and never touches the JSON-RPC
//     stdout channel.
//   - provision tools — one per provision in the resolved chain. Invoking one
//     dials that level's alpha and runs the provision in-process via OpInvoke,
//     so it reuses alpha's barriers/env/process-groups and a failure never
//     shuts alpha down.
//
// serverInstructions is surfaced to the client at `initialize` (MCP's
// instructions field) and injected into the agent's context. It is the
// top-level "what is this server for, and when should I reach for it" signal —
// scoped so the agent uses these tools for the local stack and ignores them
// elsewhere.
const serverInstructions = `zordon manages this project's local development stack — the databases, brokers, and services declared in an Alphasfile.

Use these tools when the task involves that stack: bring it up or down (start, stop), see what is running and on which ports (status, get), render the resolved config (plan), or run a provision on demand (the provision__* tools — one-off setup such as seeding, migrations, or topic creation). Provisions run inside the already-running supervisor and never tear it down; a failed provision is reported but leaves the stack running.

Prefer these tools over shelling out to ` + "`zordon`" + ` in the terminal — they return structured results and run provisions in the live supervisor. If the working directory has no Alphasfile, this project is not zordon-managed and these tools do not apply.`

func runMCP(ctx context.Context, stdio commandIO, cfg mcpConfig) error {
	log := zlog.New(stdio.Stderr, cfg.Agent)
	srv := mcp.NewServer(
		mcpImplementation(),
		&mcp.ServerOptions{Instructions: serverInstructions},
	)

	registerCommandTools(srv, cfg.Home, cfg.Agent, cfg.Test)
	registerProvisionTools(ctx, srv, log, cfg.Home, cfg.Test)

	if cfg.Transport == transportHTTP {
		return serveMCPHTTP(ctx, log, srv, cfg.Listen, cfg.AllowHosts)
	}
	log.Info("mcp", "serving over stdio")
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// mcpConfig is the typed config `zordon mcp` carries. Every value is resolved
// at flag-parse time from a flag, its ZORDON_-prefixed env var, or the
// documented default; nothing below main reads the environment again.
type mcpConfig struct {
	Transport  string
	Listen     string   // resolved bind address; empty under stdio
	AllowHosts []string // extra Host headers accepted on a loopback listener
	Home       string
	Agent      bool
	Test       alphasfile.TestConfig
}

const (
	transportStdio = "stdio"
	transportHTTP  = "http"

	// defaultMCPListen is loopback on purpose: the endpoint drives the host
	// stack and carries no authentication yet, so reaching it from elsewhere
	// has to be a deliberate --listen.
	defaultMCPListen = "127.0.0.1:7391"

	// mcpHTTPPath is where the streamable-HTTP handler is mounted; the client's
	// URL is http://<listen>/mcp.
	mcpHTTPPath = "/mcp"

	// mcpSessionTimeout closes sessions that stop making requests. stdio has one
	// session for the process lifetime, but an HTTP server outlives its clients
	// and would otherwise retain a session per client that walked away.
	mcpSessionTimeout = 30 * time.Minute

	mcpShutdownGrace = 5 * time.Second
)

// resolveMCPListen validates the transport/listen pair and returns the address
// to bind — empty under stdio, where there is nothing to bind.
func resolveMCPListen(transport, listen string) (string, error) {
	switch transport {
	case transportStdio:
		if listen != "" {
			return "", errors.New("mcp --listen: only valid with --transport=http")
		}
		return "", nil
	case transportHTTP:
	default:
		return "", fmt.Errorf("mcp --transport: unknown transport %q (want %s or %s)", transport, transportStdio, transportHTTP)
	}

	if listen == "" {
		listen = defaultMCPListen
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("mcp --listen: %q is not a host:port address", listen)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", fmt.Errorf("mcp --listen: %q is not a valid port", port)
	}
	// A wildcard bind would expose host-stack control on every interface, and
	// there is no authentication yet. Naming the interface is the guardrail
	// until there is.
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "", fmt.Errorf("mcp --listen: refusing to bind the wildcard address %q; name the interface the client reaches you on", listen)
	}
	return listen, nil
}

// validateMCPAllowHosts rejects --allow-host where it would do nothing, so a
// misplaced flag fails instead of leaving the operator to wonder why their
// sandbox still gets a 403.
func validateMCPAllowHosts(transport string, allowHosts []string) error {
	if len(allowHosts) == 0 || transport == transportHTTP {
		return nil
	}
	return errors.New("mcp --allow-host: only valid with --transport=http")
}

// serveMCPHTTP serves the same server over the streamable-HTTP transport, so an
// MCP client isolated from this host — a container or sandbox that can neither
// spawn host processes nor reach alpha's unix socket — can drive the stack with
// nothing but a URL. Every tool still executes here, host-side.
func serveMCPHTTP(ctx context.Context, log *zlog.Logger, srv *mcp.Server, addr string, allowHosts []string) error {
	// Bind before announcing: --listen with port 0 only learns its port here,
	// and the logged URL is what the operator pastes into a client config.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mcp --listen: %w", err)
	}
	boundHost := hostOnly(ln.Addr().String())
	loopback := isLoopbackHost(boundHost)

	log.Info("mcp", "serving over http on http://%s%s", ln.Addr(), mcpHTTPPath)
	if !loopback {
		log.Warn("mcp", "%s is not loopback: this endpoint controls the host stack and has no authentication", boundHost)
	}
	for _, h := range allowHosts {
		log.Info("mcp", "accepting Host header %q", h)
	}

	// WriteTimeout/ReadTimeout stay unset: streamable HTTP holds long-lived SSE
	// streams and either would sever one mid-provision. ReadHeaderTimeout is
	// what bounds a slow-header client.
	httpSrv := &http.Server{
		Handler:           mcpHandler(srv, loopback, allowHosts),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// stdio ends when the client closes the pipe; HTTP has no such signal, so
	// the process has to take one.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-sigCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(sigCtx), mcpShutdownGrace)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			_ = httpSrv.Close()
		}
	}()

	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("mcp http: %w", err)
	}
	return nil
}

// mcpHandler is the served HTTP surface: the streamable transport at /mcp,
// behind the Host guard, and nothing else. Kept apart from binding and serving
// so the guard can be exercised over a test server rather than a real listener.
func mcpHandler(srv *mcp.Server, listenerLoopback bool, allowHosts []string) http.Handler {
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		// The SDK's own guard is switched off in favour of checkHost, which
		// enforces the same rule and additionally honors --allow-host.
		&mcp.StreamableHTTPOptions{SessionTimeout: mcpSessionTimeout, DisableLocalhostProtection: true},
	)
	mux := http.NewServeMux()
	mux.Handle(mcpHTTPPath, checkHost(handler, listenerLoopback, allowHosts))
	return mux
}

// checkHost is DNS-rebinding protection, widened by --allow-host. A browser on
// this machine can be talked into POSTing to 127.0.0.1, so a loopback listener
// accepts only a loopback Host header — unless the operator named the hostname
// their sandbox dials (`host.docker.internal` and friends), which no browser
// would send by accident. A non-loopback bind was already a deliberate act, so
// it imposes nothing further.
func checkHost(next http.Handler, listenerLoopback bool, allowHosts []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(listenerLoopback, r.Host, allowHosts) {
			http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q (pass --allow-host %s to accept it)", r.Host, hostOnly(r.Host)), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hostAllowed(listenerLoopback bool, reqHost string, allowHosts []string) bool {
	if !listenerLoopback {
		return true
	}
	host := hostOnly(reqHost)
	return isLoopbackHost(host) || slices.Contains(allowHosts, host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostOnly strips the port from a `host:port`, tolerating a bare host (a Host
// header carries no port when the client used the scheme's default).
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func registerCommandTools(srv *mcp.Server, zordonHome string, agent bool, testCfg alphasfile.TestConfig) {
	root, _ := buildRootCommand(commandIO{Stdout: nil, Stderr: nil}) // introspection only
	for _, sub := range commandToolSubcommands(root) {
		name := sub.Name
		mcp.AddTool(srv, &mcp.Tool{
			Name:        name,
			Description: commandToolDescription(sub),
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in commandToolInput) (*mcp.CallToolResult, any, error) {
			return runCommandTool(ctx, zordonHome, agent, testCfg, name, in.Args)
		})
	}
}

// commandToolSubcommands is the single source of truth for which subcommands
// become MCP tools: every one except `mcp` itself (can't serve an MCP server as
// a tool inside MCP). Keeping it a function lets a test pin the surface so the
// tool list can never silently drift from the CLI.
func commandToolSubcommands(root *ff.Command) []*ff.Command {
	var out []*ff.Command
	for _, sub := range root.Subcommands {
		if sub.Name == "mcp" {
			continue
		}
		out = append(out, sub)
	}
	return out
}

func commandToolDescription(sub *ff.Command) string {
	var b strings.Builder
	b.WriteString(sub.ShortHelp)
	fmt.Fprintf(&b, "\nUsage: %s", sub.Usage)
	b.WriteString("\nPass flags and positional args via the `args` array; call with [\"-h\"] for the full flag list.")
	return b.String()
}

// runCommandTool re-runs one subcommand against capture buffers and returns its
// output as text. Output stays in the buffers — it must never reach os.Stdout,
// which is the JSON-RPC channel.
func runCommandTool(ctx context.Context, zordonHome string, agent bool, testCfg alphasfile.TestConfig, name string, args []string) (*mcp.CallToolResult, any, error) {
	var out, errBuf bytes.Buffer
	root, _ := buildRootCommand(commandIO{Stdout: &out, Stderr: &errBuf})

	argv := commandArgv(zordonHome, agent, testCfg, name, args)
	err := root.ParseAndRun(ctx, argv, ff.WithEnvVarPrefix("ZORDON"))

	text := joinOutput(out.String(), errBuf.String())
	isErr := false
	switch {
	case errors.Is(err, ff.ErrHelp), errors.Is(err, ff.ErrNoExec):
		if sub := findSubcommand(root, name); sub != nil {
			text = joinOutput(text, ffhelp.Command(sub).String())
		}
	case err != nil:
		text = joinOutput(text, "error: "+err.Error())
		isErr = true
	}
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: isErr,
	}, nil, nil
}

// commandArgv reconstructs the root flags the MCP server itself resolved so the
// rebuilt tree sees the same home / test-harness context, then appends the
// subcommand and its tool args.
func commandArgv(zordonHome string, agent bool, testCfg alphasfile.TestConfig, name string, args []string) []string {
	var argv []string
	if zordonHome != "" {
		argv = append(argv, "--home", zordonHome)
	}
	if agent {
		argv = append(argv, "--agent")
	}
	if testCfg.Harness {
		argv = append(argv, "--test-harness")
		if testCfg.LogPath != "" {
			argv = append(argv, "--test-log", testCfg.LogPath)
		}
	}
	argv = append(argv, name)
	return append(argv, args...)
}

func registerProvisionTools(ctx context.Context, srv *mcp.Server, log *zlog.Logger, zordonHome string, testCfg alphasfile.TestConfig) {
	levels, err := resolveChain(ctx, zordonHome, testCfg)
	if err != nil {
		// No chain (e.g. run outside a project) — still serve command tools.
		log.Warn("mcp", "provision tools unavailable: %v", err)
		return
	}
	seen := map[string]struct{}{}
	count := 0
	for _, lv := range levels {
		if lv.state == nil {
			continue
		}
		socket := lv.inv.SocketPath()
		for _, p := range mcpserver.Provisions(lv.state.Services) {
			name := mcpserver.Unique(seen, p.ToolName())
			provID, sock := p.ID, socket
			// Dynamic per-provision schema: declared arguments become typed
			// fields, so the non-generic AddTool (which takes a manual schema)
			// is used instead of the generic, fixed-input one.
			srv.AddTool(&mcp.Tool{
				Name:        name,
				Description: p.Description(),
				InputSchema: p.InputSchema(),
			}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				args, env := splitProvisionInput(req)
				return invokeProvisionTool(ctx, sock, provID, args, env), nil
			})
			count++
		}
	}
	log.Info("mcp", "registered %d provision tool(s)", count)
}

// splitProvisionInput unmarshals a provision tool call into its declared
// arguments and the generic `env` overlay. Unknown / type mismatches are left
// to alpha (validated against the declared arguments there).
func splitProvisionInput(req *mcp.CallToolRequest) (args map[string]any, env map[string]string) {
	raw := map[string]any{}
	if len(req.Params.Arguments) > 0 {
		_ = json.Unmarshal(req.Params.Arguments, &raw)
	}
	env = map[string]string{}
	if e, ok := raw["env"].(map[string]any); ok {
		for k, v := range e {
			env[k] = fmt.Sprintf("%v", v)
		}
	}
	delete(raw, "env")
	return raw, env
}

// invokeProvisionTool dials the level's alpha, sends OpInvoke with the supplied
// arguments + env, and streams the provision's output back as the tool result.
// A failed provision (or a missing alpha) yields an error result (IsError) —
// never a torn-down alpha and never a protocol-level error.
func invokeProvisionTool(ctx context.Context, socket, provID string, args map[string]any, env map[string]string) *mcp.CallToolResult {
	conn, err := control.Dial(socket, 1*time.Second)
	if err != nil {
		if control.IsNotRunning(err) {
			return errorResult("alpha is not running for this provision; run `zordon start` first")
		}
		return errorResult(fmt.Sprintf("dial alpha: %v", err))
	}
	defer conn.Close()

	// Let a cancelled tool call unblock the stream read.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	enc := protocol.NewEncoder(conn)
	dec := protocol.NewDecoder(conn)
	if err := enc.Write(&protocol.Request{Op: protocol.OpInvoke, Invoke: &protocol.InvokeArgs{Provision: provID, Args: args, Env: env}}); err != nil {
		return errorResult(fmt.Sprintf("send invoke: %v", err))
	}

	var b strings.Builder
	for {
		var ev protocol.Event
		if err := dec.Read(&ev); err != nil {
			// Stream died without a verdict (alpha shut down / crashed). Report
			// what we have; the caller sees IsError.
			return errorResult(b.String() + fmt.Sprintf("stream closed before completion: %v", err))
		}
		switch ev.Kind {
		case protocol.EventLog:
			if ev.Stream != "" && ev.Stream != "stdout" {
				fmt.Fprintf(&b, "[%s] %s\n", ev.Stream, ev.Line)
			} else {
				fmt.Fprintln(&b, ev.Line)
			}
		case protocol.EventDone:
			return textResult(b.String())
		case protocol.EventError:
			if ev.Error != "" {
				fmt.Fprintf(&b, "error: %s\n", ev.Error)
			}
			return errorResult(b.String())
		default:
			if ev.Line != "" {
				fmt.Fprintln(&b, ev.Line)
			}
		}
	}
}

func textResult(s string) *mcp.CallToolResult {
	if strings.TrimSpace(s) == "" {
		s = "(provision produced no output)"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errorResult(s string) *mcp.CallToolResult {
	r := textResult(s)
	r.IsError = true
	return r
}

func findSubcommand(root *ff.Command, name string) *ff.Command {
	for _, sub := range root.Subcommands {
		if sub.Name == name {
			return sub
		}
	}
	return nil
}

func joinOutput(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(p)
	}
	return b.String()
}
