//go:build conformance_go

// Conformance tests for the `debugger { ... }` macro on `service "go"`.
//
// The macro is a compile-time expansion of an Alphasfile block into
// three downstream effects: toolchain.tools injection (dlv + mcp-dap-
// server), build flag injection (-gcflags='all=-N -l'), and runtime
// wrapping (`dlv exec --headless --listen=… -- <binary>`). Plus one
// structural side-effect: a synthetic `agent.mcp.debug` feature on
// the service for the (forthcoming) MCP bridge.
//
// Tests are split by cost:
//
//   - Validation tests run zordon start against an Alphasfile that
//     fails the macro's parse-time checks. zordon aborts BEFORE alpha
//     is spawned, so no mise/Go install runs — these are sub-second.
//   - Behavior tests run a full bringup with dlv actually wrapping the
//     service. Cold-cache cost includes installing dlv + mcp-dap-
//     server via `go install`; budget aligns with the rest of this
//     package's cold-run budget (15 min ceiling).
//
// The Go toolchain version pinned in these tests matches the rest of
// tests/conformance/ so the mise cache is shared across files.
package conformance_test

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// --- validation failures (fast: fail before alpha spawn) ------------

// TestDebugger_rejectsExplicitRuntimeCmd pins the per-service
// validation: `debugger { enabled = true; wrap_runtime = true }` (the
// default) wraps the runtime itself, so an explicit `runtime { cmd =
// […] }` is unreachable. The macro refuses to silently overwrite the
// user's cmd or silently skip the wrap — both would be a surprise.
// Hint in the error nudges toward the two correct paths (`arguments`
// or `wrap_runtime = false`).
func TestDebugger_rejectsExplicitRuntimeCmd(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  debugger {
    enabled = true
  }
}
`)

	p.Start(t, zordontest.StartTimeout(30*time.Second)).
		Failed().
		OutputContains(
			`debugger.enabled = true`,
			`incompatible with an explicit`,
			`runtime.cmd`,
			`wrap_runtime = false`,
		)
}

// TestDebugger_rejectsNonGoToolchain pins the toolchain-scoped
// validation: the macro's effects are dlv-specific (build flags,
// runtime wrapping), so it has nothing to do on a Rust or Ruby
// service. Rather than silently no-op, the parser refuses — keeps
// the door open for `service "rust" { debugger { … } }` to mean
// `rust-gdb` / `lldb` once implemented, without breaking the
// existing escape route.
func TestDebugger_rejectsNonGoToolchain(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]

service "rust" "tansu" {
  crate {
    name    = "tansu"
    version = "0.1.0"
  }

  debugger {
    enabled = true
  }
}
`)

	p.Start(t, zordontest.StartTimeout(30*time.Second)).
		Failed().
		OutputContains(`debugger`, `only supported on service "go"`)
}

// TestDebugger_requiresToolchainBlock pins the dependency: `debugger {
// enabled = true }` is asking for `dlv + mcp-dap-server` to be
// installed into a *specific* Go toolchain version. With no `toolchain
// { go { … } }` block, there's nothing to install into — the macro
// fails the Alphasfile rather than silently picking the host's Go.
func TestDebugger_requiresToolchainBlock(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }
  vars = { port = net::pickport() }
  arguments { values = { main = { addr = "127.0.0.1:${self.vars.port}" } } }
  debugger { enabled = true }
}
`)

	p.Start(t, zordontest.StartTimeout(30*time.Second)).
		Failed().
		OutputContains(`debugger`, `toolchain`, `go`)
}

// --- behavior (slow: cold-cache install + dlv-wrapped bringup) -------

// TestDebugger_happyPath_wrapsRuntimeAndExposesDAP is the
// load-bearing positive proof for the whole macro. After bringup:
//
//   - The inferior (echo) serves HTTP on its picked port — proves the
//     runtime wrap didn't break dlv's `--continue` path / readiness.
//   - dlv's headless DAP listener answers on the macro-resolved
//     debugger.port — proves the wrap actually started dlv, not just
//     prefixed argv with something innocuous.
//   - `service.go.svc1.agent.mcp.debug.address` equals
//     "127.0.0.1:<port>" — proves the structural MCP feature is
//     wired through the wire-stable Service / `zordon get` projection.
//
// Port is *picked*, not pinned: the test reads it back via `zordon
// get` rather than asserting a fixed value, mirroring the real IDE
// workflow.
//
// Cold-cache budget: shared with the rest of this package.
func TestDebugger_happyPath_wrapsRuntimeAndExposesDAP(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  arguments {
    values = {
      main = {
        addr = "127.0.0.1:${self.vars.port}"
      }
    }
  }

  debugger {
    enabled = true
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`)

	mustStart(t, p)
	httpPort := p.Get(t, "service.go.svc1.vars.port").Int()
	mustGetEcho(t, httpPort)

	// Read back the picked port — the macro defaults to net::pickport
	// semantics, so the literal value isn't predictable but it MUST
	// be a valid TCP port and equal to what agent.mcp.debug.address
	// advertises.
	dapPort := p.Get(t, "service.go.svc1.debugger.port").Int()
	if dapPort <= 0 || dapPort > 65535 {
		t.Fatalf("debugger.port = %d; want a picked TCP port (1-65535)", dapPort)
	}
	gotAddr := p.Get(t, "service.go.svc1.agent.mcp.debug.address").String()
	wantAddr := fmt.Sprintf("127.0.0.1:%d", dapPort)
	if gotAddr != wantAddr {
		t.Errorf("agent.mcp.debug.address = %q; want %q", gotAddr, wantAddr)
	}
	if err := dapInitialize(wantAddr); err != nil {
		t.Errorf("dlv DAP listener not responding on %s: %v", wantAddr, err)
	}
}

// TestDebugger_explicitPortPinHonored: when the user writes a literal
// `port = N`, the macro respects it instead of picking. Covers the
// IDE-stability case where launch.json hardcodes a port and the user
// wants the manifest to match.
func TestDebugger_explicitPortPinHonored(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	const dapPort = 2347
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  arguments {
    values = {
      main = {
        addr = "127.0.0.1:${self.vars.port}"
      }
    }
  }

  debugger {
    enabled = true
    port    = %d
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, dapPort))

	mustStart(t, p)
	if gotPort := p.Get(t, "service.go.svc1.debugger.port").Int(); gotPort != dapPort {
		t.Errorf("debugger.port = %d; want %d (explicit pin)", gotPort, dapPort)
	}
	if err := dapInitialize(fmt.Sprintf("127.0.0.1:%d", dapPort)); err != nil {
		t.Errorf("dlv DAP listener not responding on pinned port %d: %v", dapPort, err)
	}
}

// TestDebugger_multipleServicesGetDistinctPorts pins the corollary
// of the picked-port default: two services with `debugger { enabled =
// true }` in the SAME stack each get their own listener with no
// configuration. This is the failure case 2345-as-default created
// (collision) and what we're explicitly designing against.
func TestDebugger_multipleServicesGetDistinctPorts(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")
	p.CopyTree("golden/go/echo", "src/svc2")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }
  vars = { port = net::pickport() }
  arguments { values = { main = { addr = "127.0.0.1:${self.vars.port}" } } }
  debugger { enabled = true }
  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}

service "go" "svc2" {
  src {
    path = "./src/svc2"
    exe = "."
  }
  vars = { port = net::pickport() }
  arguments { values = { main = { addr = "127.0.0.1:${self.vars.port}" } } }
  debugger { enabled = true }
  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`)

	mustStart(t, p)
	a := p.Get(t, "service.go.svc1.debugger.port").Int()
	if a <= 0 {
		t.Fatalf("svc1 debugger.port = %d; want a picked TCP port", a)
	}
	b := p.Get(t, "service.go.svc2.debugger.port").Int()
	if b <= 0 {
		t.Fatalf("svc2 debugger.port = %d; want a picked TCP port", b)
	}
	if a == b {
		t.Errorf("two debugger-enabled services got the same port %d — picked-port default failed to isolate", a)
	}
	if err := dapInitialize(fmt.Sprintf("127.0.0.1:%d", a)); err != nil {
		t.Errorf("svc1 dlv DAP listener not responding on %d: %v", a, err)
	}
	if err := dapInitialize(fmt.Sprintf("127.0.0.1:%d", b)); err != nil {
		t.Errorf("svc2 dlv DAP listener not responding on %d: %v", b, err)
	}
}

// TestDebugger_mcpFalseSuppressesAgentFeature: setting mcp = false on
// the debugger block keeps dlv wrapping the runtime (build flags +
// runtime wrap still apply) but suppresses the synthetic
// `agent.mcp.debug` feature. The catalog stays catalog — alpha won't
// advertise a debug capability to agents.
//
// We assert dlv is STILL running (DAP answers) so the test
// distinguishes "MCP feature absent" from "the whole macro failed".
func TestDebugger_mcpFalseSuppressesAgentFeature(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  arguments {
    values = {
      main = {
        addr = "127.0.0.1:${self.vars.port}"
      }
    }
  }

  debugger {
    enabled = true
    mcp     = false
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`)

	mustStart(t, p)
	dapPort := p.Get(t, "service.go.svc1.debugger.port").Int()

	// dlv still wrapping: DAP listener answers.
	if err := dapInitialize(fmt.Sprintf("127.0.0.1:%d", dapPort)); err != nil {
		t.Errorf("dlv DAP listener not responding on %d (macro half-broken?): %v", dapPort, err)
	}

	// `zordon get` on the agent.mcp.debug.address path should fail —
	// the feature isn't in the catalog when mcp = false. We use the
	// raw run (not p.Get, which fatals on non-zero exit) so the
	// expected miss is asserted as an exit code, not a panic.
	res := p.Zordon("get", "service.go.svc1.agent.mcp.debug.address").Run(t)
	if res.ExitCode == 0 {
		t.Errorf("agent.mcp.debug.address resolved to %q despite mcp = false; want lookup to fail", strings.TrimSpace(res.Stdout))
	}
}

// --- helpers ---------------------------------------------------------

// dapInitialize speaks one DAP `initialize` request and verifies the
// peer replies with a Content-Length-prefixed frame — same protocol
// VS Code / GoLand use when attaching. A successful frame back is
// strong evidence dlv is alive on the socket; we don't fully parse
// the JSON, the header is sufficient to distinguish a real DAP
// server from any TCP listener.
func dapInitialize(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	body := `{"seq":1,"type":"request","command":"initialize","arguments":{"adapterID":"go","clientID":"zordon-conformance","linesStartAt1":true,"columnsStartAt1":true,"pathFormat":"path"}}`
	msg := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	if _, err := conn.Write([]byte(msg)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "Content-Length:") {
		return fmt.Errorf("unexpected reply, want DAP frame header: %q", buf[:n])
	}
	return nil
}
