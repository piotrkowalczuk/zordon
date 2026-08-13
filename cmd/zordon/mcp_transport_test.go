package main

import (
	"io"
	"strings"
	"testing"

	"github.com/peterbourgon/ff/v4"
)

func TestResolveMCPListen(t *testing.T) {
	cases := map[string]struct {
		transport string
		listen    string
		want      string
		wantErr   string
	}{
		"stdio binds nothing":        {transport: "stdio"},
		"http defaults to loopback":  {transport: "http", want: defaultMCPListen},
		"http honors an address":     {transport: "http", listen: "192.168.65.2:9000", want: "192.168.65.2:9000"},
		"http accepts an ephemeral":  {transport: "http", listen: "127.0.0.1:0", want: "127.0.0.1:0"},
		"http accepts a bracketed 6": {transport: "http", listen: "[::1]:7391", want: "[::1]:7391"},

		"listen needs http":   {transport: "stdio", listen: "127.0.0.1:7391", wantErr: "--listen"},
		"unknown transport":   {transport: "bogus", wantErr: "--transport"},
		"empty transport":     {transport: "", wantErr: "--transport"},
		"not a host:port":     {transport: "http", listen: "nonsense", wantErr: "--listen"},
		"port not a number":   {transport: "http", listen: "127.0.0.1:http", wantErr: "--listen"},
		"port out of range":   {transport: "http", listen: "127.0.0.1:70000", wantErr: "--listen"},
		"wildcard v4 refused": {transport: "http", listen: "0.0.0.0:7391", wantErr: "wildcard"},
		"wildcard v6 refused": {transport: "http", listen: "[::]:7391", wantErr: "wildcard"},
		"empty host refused":  {transport: "http", listen: ":7391", wantErr: "wildcard"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := resolveMCPListen(c.transport, c.listen)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveMCPListen(%q, %q) = %q, nil; want an error mentioning %q", c.transport, c.listen, got, c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error = %q; want it to mention %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveMCPListen(%q, %q) errored: %v", c.transport, c.listen, err)
			}
			if got != c.want {
				t.Errorf("resolveMCPListen(%q, %q) = %q; want %q", c.transport, c.listen, got, c.want)
			}
		})
	}
}

func TestHostAllowed(t *testing.T) {
	cases := map[string]struct {
		listenerLoopback bool
		reqHost          string
		allowHosts       []string
		want             bool
	}{
		"loopback listener takes loopback ip":   {listenerLoopback: true, reqHost: "127.0.0.1:7391", want: true},
		"loopback listener takes localhost":     {listenerLoopback: true, reqHost: "localhost:7391", want: true},
		"loopback listener takes ipv6 loopback": {listenerLoopback: true, reqHost: "[::1]:7391", want: true},
		"loopback listener takes portless host": {listenerLoopback: true, reqHost: "localhost", want: true},

		"loopback listener rejects a name": {listenerLoopback: true, reqHost: "host.docker.internal:7391", want: false},
		"loopback listener rejects rebind": {listenerLoopback: true, reqHost: "evil.example.com", want: false},
		"allow-host admits the named sandbox": {
			listenerLoopback: true, reqHost: "host.docker.internal:7391",
			allowHosts: []string{"host.docker.internal"}, want: true,
		},
		"allow-host does not admit others": {
			listenerLoopback: true, reqHost: "evil.example.com:7391",
			allowHosts: []string{"host.docker.internal"}, want: false,
		},

		// A non-loopback bind was already deliberate, so it adds no Host rule.
		"routable listener takes any host": {listenerLoopback: false, reqHost: "evil.example.com", want: true},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := hostAllowed(c.listenerLoopback, c.reqHost, c.allowHosts); got != c.want {
				t.Errorf("hostAllowed(%v, %q, %v) = %v; want %v", c.listenerLoopback, c.reqHost, c.allowHosts, got, c.want)
			}
		})
	}
}

func TestValidateMCPAllowHosts(t *testing.T) {
	cases := map[string]struct {
		transport  string
		allowHosts []string
		wantErr    bool
	}{
		"http with hosts":  {transport: "http", allowHosts: []string{"host.docker.internal"}},
		"http without":     {transport: "http"},
		"stdio without":    {transport: "stdio"},
		"stdio with hosts": {transport: "stdio", allowHosts: []string{"host.docker.internal"}, wantErr: true},
		"bogus with hosts": {transport: "bogus", allowHosts: []string{"x"}, wantErr: true},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			err := validateMCPAllowHosts(c.transport, c.allowHosts)
			if c.wantErr && err == nil {
				t.Fatalf("validateMCPAllowHosts(%q, %v) = nil; want an error", c.transport, c.allowHosts)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("validateMCPAllowHosts(%q, %v) errored: %v", c.transport, c.allowHosts, err)
			}
			if c.wantErr && !strings.Contains(err.Error(), "--allow-host") {
				t.Errorf("error = %q; want it to mention '--allow-host'", err)
			}
		})
	}
}

// An unusable transport/listen pair must fail before the server starts, so the
// operator sees why instead of a server bound somewhere unintended. Both cases
// are rejected during Exec, which also keeps the test from blocking on a live
// server.
func TestMCP_transportInvalid(t *testing.T) {
	err := runMCPArgs(t, "--transport", "bogus")
	if err == nil {
		t.Fatal("mcp --transport bogus returned nil; want a transport error")
	}
	if !strings.Contains(err.Error(), "--transport") {
		t.Errorf("error = %q; want it to mention '--transport'", err)
	}
}

func TestMCP_listenWithStdio(t *testing.T) {
	err := runMCPArgs(t, "--transport", "stdio", "--listen", "127.0.0.1:7391")
	if err == nil {
		t.Fatal("mcp --transport stdio --listen returned nil; want a rejected flag")
	}
	if !strings.Contains(err.Error(), "--listen") {
		t.Errorf("error = %q; want it to mention '--listen'", err)
	}
}

func TestMCP_allowHostWithStdio(t *testing.T) {
	err := runMCPArgs(t, "--allow-host", "host.docker.internal")
	if err == nil {
		t.Fatal("mcp --allow-host under stdio returned nil; want a rejected flag")
	}
	if !strings.Contains(err.Error(), "--allow-host") {
		t.Errorf("error = %q; want it to mention '--allow-host'", err)
	}
}

func TestMCP_listenWildcard(t *testing.T) {
	err := runMCPArgs(t, "--transport", "http", "--listen", "0.0.0.0:7391")
	if err == nil {
		t.Fatal("mcp --listen 0.0.0.0:7391 returned nil; want a refused wildcard bind")
	}
	if !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("error = %q; want it to mention the refused wildcard", err)
	}
}

// runMCPArgs drives the real command tree the way main does, so the flags under
// test are parsed and validated exactly as they are in production.
func runMCPArgs(t *testing.T, args ...string) error {
	t.Helper()
	root, _ := buildRootCommand(commandIO{Stdout: io.Discard, Stderr: io.Discard})
	return root.ParseAndRun(t.Context(), append([]string{"mcp"}, args...), ff.WithEnvVarPrefix("ZORDON"))
}
