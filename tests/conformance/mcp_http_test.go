// MCP-over-HTTP conformance: `zordon mcp --transport=http` serves the same
// tool set as the stdio server, but as a host-side network service an isolated
// client reaches by URL. The contract under test:
//
//   - it announces the address it bound, which is the only way to use port 0
//     and the line an operator pastes into a client config;
//   - the tool surface is served whether or not alpha is running, because the
//     agent is expected to call `start` through it;
//   - the endpoint lives at /mcp and nowhere else;
//   - on a loopback bind only a loopback Host header is accepted, unless the
//     operator named the host their sandbox dials via --allow-host.
//
// Untagged, like plan: none of this needs a toolchain, a build, or a running
// alpha, so it runs in every CI leg rather than once per toolchain.
package conformance_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// mcpAlphasfile is a chain that resolves statically — `package` is never
// fetched — so the server can register provision tools without a toolchain.
const mcpAlphasfile = `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

service "go" "app" {
  package = "example.com/app@v0.0.0"

  vars = {
    port = net::pickport()
  }

  runtime {
    provision "seed" {
      after = never
      cmd   = "true"
    }
  }
}
`

// The whole point of the http transport is that the agent drives a stack it
// may also have to start. So the tools must be there with alpha down — a
// server that only works once the stack is up would be useless to a sandboxed
// client, which cannot start it any other way.
func TestMCPHTTP_servesToolsWithoutAlpha(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", mcpAlphasfile)

	srv := p.MCPHTTP(t)

	if !strings.HasSuffix(srv.URL(), "/mcp") {
		t.Errorf("announced URL = %q; want it to end in /mcp", srv.URL())
	}
	if got := zordontest.PostMCP(t, srv.URL(), ""); got != http.StatusOK {
		t.Fatalf("initialize = %d; want %d\nstderr:\n%s", got, http.StatusOK, srv.Stderr())
	}
	// The provision came from static evaluation, with no alpha to ask.
	if !strings.Contains(srv.Stderr(), "registered 1 provision tool(s)") {
		t.Errorf("provision tools not registered with alpha down\nstderr:\n%s", srv.Stderr())
	}
}

func TestMCPHTTP_servesOnlyTheMCPPath(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", mcpAlphasfile)

	srv := p.MCPHTTP(t)
	base := strings.TrimSuffix(srv.URL(), "/mcp")

	cases := map[string]string{
		"root":        base + "/",
		"nested":      base + "/mcp/extra",
		"look-alike":  base + "/mcpx",
		"other route": base + "/health",
	}
	for hint, url := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := zordontest.PostMCP(t, url, ""); got != http.StatusNotFound {
				t.Errorf("POST %s = %d; want %d — a misconfigured client URL must fail loudly", url, got, http.StatusNotFound)
			}
		})
	}
}

// A loopback listener is reachable by any web page on the machine, so it
// accepts only a loopback Host — DNS-rebinding protection. A container dials
// the host by name, which is what --allow-host is for. This drives the real
// binary because that is the only way to prove the flag reaches the guard.
func TestMCPHTTP_hostGuard(t *testing.T) {
	const allowed = "host.docker.internal"

	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", mcpAlphasfile)

	guarded := p.MCPHTTP(t)
	if got := zordontest.PostMCP(t, guarded.URL(), allowed+":7391"); got != http.StatusForbidden {
		t.Errorf("without --allow-host, Host %q = %d; want %d", allowed, got, http.StatusForbidden)
	}
	if got := zordontest.PostMCP(t, guarded.URL(), ""); got != http.StatusOK {
		t.Errorf("loopback Host = %d; want %d", got, http.StatusOK)
	}

	allowing := p.MCPHTTP(t, "--allow-host", allowed)
	cases := map[string]struct {
		host string
		want int
	}{
		"the named host is admitted": {host: allowed + ":7391", want: http.StatusOK},
		"loopback still works":       {host: "", want: http.StatusOK},
		"an unlisted name is not":    {host: "evil.example.com", want: http.StatusForbidden},
		"nor is a rebind attempt":    {host: "attacker.test:1234", want: http.StatusForbidden},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := zordontest.PostMCP(t, allowing.URL(), c.host); got != c.want {
				t.Errorf("Host %q = %d; want %d\nstderr:\n%s", c.host, got, c.want, allowing.Stderr())
			}
		})
	}
}
