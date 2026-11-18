package alphasfile

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlock"
	"github.com/piotrkowalczuk/zordon/internal/znet"
	"github.com/piotrkowalczuk/zordon/internal/ztest"
)

func TestPlan_Compute_pickportPopulatesServicePortsWithoutCollision(t *testing.T) {
	af := compile(t, `
toolchain {
  go { version = "1.26.2" }
}

service "go" "api" {
  package = "example.com/api@v1.0.0"
  vars = {
    http_port = net::pickport()
    grpc_port = net::pickport()
  }
  debugger {
    enabled = true
    wrap_runtime = false
  }
}

service "go" "worker" {
  package = "example.com/worker@v1.0.0"
  env = {
    API_HTTP = "127.0.0.1:${service.go.api.vars.http_port}"
  }
  vars = {
    metrics_port = net::pickport()
  }
}
`, nil)

	api := svcByName(af, "api")
	if api == nil {
		t.Fatal("service api not found")
	}
	if len(api.Ports) != 3 {
		t.Fatalf("api.Ports = %v, want 3 ports (http, grpc, debugger)", api.Ports)
	}
	if api.Debugger == nil || !slices.Contains(api.Ports, api.Debugger.Port) {
		t.Fatalf("api.Ports = %v does not contain debugger port %v", api.Ports, api.Debugger)
	}

	worker := svcByName(af, "worker")
	if worker == nil {
		t.Fatal("service worker not found")
	}
	if len(worker.Ports) != 1 {
		t.Fatalf("worker.Ports = %v, want 1 port (must not inherit api's ports)", worker.Ports)
	}

	seen := map[int]bool{}
	for _, p := range append(slices.Clone(api.Ports), worker.Ports...) {
		if p < znet.PortRangeBase || p >= znet.PortRangeBase+znet.PortRangeSpan {
			t.Errorf("port %d outside [%d, %d)", p, znet.PortRangeBase, znet.PortRangeBase+znet.PortRangeSpan)
		}
		if seen[p] {
			t.Errorf("duplicate port %d", p)
		}
		seen[p] = true
	}
}

func TestPlan_Compute_sharedReadinessPortOwnedOnlyByAllocator(t *testing.T) {
	af := compile(t, `
service "go" "db" {
  package = "example.com/db@v1.0.0"
  vars = { port = net::pickport() }
  readiness {
    tcp { port = self.vars.port }
  }
}

service "go" "app" {
  package = "example.com/app@v1.0.0"
  readiness {
    tcp { port = service.go.db.vars.port }
  }
}
`, nil)

	db := svcByName(af, "db")
	if db == nil || len(db.Ports) != 1 {
		t.Fatalf("db.Ports = %v, want 1 port", db.Ports)
	}
	app := svcByName(af, "app")
	if app == nil {
		t.Fatal("service app not found")
	}
	if len(app.Ports) != 0 {
		t.Fatalf("app.Ports = %v, want empty (app must not claim db's port via readiness probe)", app.Ports)
	}
}

func TestPlan_Compute_skipsWorkspacePort(t *testing.T) {
	inv := testInv()
	wsPort := WorkspacePort(inv.FsHash)

	m, err := NewManifestState("test.hcl", []byte(`
service "go" "api" {
  package = "example.com/api@v1.0.0"
  vars = { port = net::pickport() }
}
`), inv)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := m.Plan(nil, testCfgHash, TestConfig{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for i := range znet.PortRangeSpan {
		p := znet.PortRangeBase + i
		if p != wsPort {
			plan.r.ports.Exclude(p)
		}
	}

	if _, err := plan.Compute(); err == nil || !strings.Contains(err.Error(), "no free port available") {
		t.Fatalf("Compute when only WorkspacePort(%q)=%d is unpicked: err = %v, want no free port available", inv.FsHash, wsPort, err)
	}
}

func TestPlan_ReservePorts_holdsLockAcrossHandOff(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	kc := zlock.NewGlobalKeychain(t.TempDir())
	inv := testInv()

	m, err := NewManifestState("test.hcl", []byte(`
service "go" "api" {
  package = "example.com/api@v1.0.0"
  vars = { port = net::pickport() }
}
`), inv)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := m.Plan(nil, testCfgHash, TestConfig{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	ports := plan.ReservePorts(kc)
	defer ports.Close()

	af, err := plan.Compute()
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	api := svcByName(af, "api")
	if api == nil || len(api.Ports) != 1 {
		t.Fatalf("api.Ports = %v, want 1 port", api.Ports)
	}
	port := api.Ports[0]

	if _, err := kc.TryLockPort(port); !errors.Is(err, zlock.ErrLocked) {
		t.Fatalf("TryLockPort(%d) while held after Compute = %v, want ErrLocked", port, err)
	}

	ports.HandOff()

	holder, err := znet.HoldPort(kc, port)
	if err != nil {
		t.Fatalf("HoldPort(%d) after ports.HandOff: %v", port, err)
	}
	holder.Close()

	unlock, err := kc.TryLockPort(port)
	if err != nil {
		t.Fatalf("TryLockPort(%d) after holder.Close: %v", port, err)
	}
	unlock()
}

func TestPlan_Compute_discardsReservationsOnComputeError(t *testing.T) {
	kc := zlock.NewGlobalKeychain(t.TempDir())
	inv := testInv()

	res, err := znet.ReservePort(kc, nil)
	if err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	targetPort := res.Port()
	res.Close()

	m, err := NewManifestState("test.hcl", []byte(`
service "go" "api" {
  package = "example.com/api@v1.0.0"
  vars = { port = net::pickport() }
}

service "unknown_tc" "broken" {
  package = "example.com/broken@v1.0.0"
}
`), inv)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := m.Plan(nil, testCfgHash, TestConfig{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	plan.ReservePorts(kc)
	for i := range znet.PortRangeSpan {
		p := znet.PortRangeBase + i
		if p != targetPort {
			plan.r.ports.Exclude(p)
		}
	}

	if _, err := plan.Compute(); err == nil {
		t.Fatal("Compute succeeded on invalid toolchain; want error")
	}

	unlock, err := kc.TryLockPort(targetPort)
	if err != nil {
		t.Fatalf("TryLockPort(%d) after failed Compute: %v (lock leaked on error)", targetPort, err)
	}
	unlock()
}

func TestPlan_Compute_claimsUnownedPortFromPackageInputViaReadiness(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" { inputs = { http_port = net::pickport() } }`,
		"web/Alphasfile": `
package "web" {
  inputs = {
    http_port  = { description = "HTTP port.", type = number }
    admin_port = { description = "Admin port.", type = number, default = net::pickport() }
  }

  component "web" {
    service "go" "web" {
      package = "example.com/web@v1.0.0"
      vars = { port = inputs.http_port }
      readiness {
        tcp { port = inputs.admin_port }
      }
    }
  }
}
`,
	})
	af := openTree(t, root)
	svc := svcByName(af, "web/web/web")
	if svc == nil {
		t.Fatal("service web/web/web not found")
	}
	if len(svc.Ports) != 1 {
		t.Fatalf("svc.Ports = %v, want 1 port (admin_port from readiness)", svc.Ports)
	}
}

func TestPlan_Compute_unownedPortReadinessTakesPriorityOverClientVar(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" {}`,
		"web/Alphasfile": `
package "web" {
  inputs = {
    port = { description = "Port.", type = number, default = net::pickport() }
  }

  component "web" {
    service "go" "aclient" {
      package = "example.com/aclient@v1.0.0"
      vars = { upstream = "http://127.0.0.1:${inputs.port}" }
    }

    service "go" "server" {
      package = "example.com/server@v1.0.0"
      vars = { port = inputs.port }
      readiness {
        tcp { port = inputs.port }
      }
    }
  }
}
`,
	})
	af := openTree(t, root)
	aclient := svcByName(af, "web/web/aclient")
	if aclient == nil {
		t.Fatal("service web/web/aclient not found")
	}
	if len(aclient.Ports) != 0 {
		t.Fatalf("aclient.Ports = %v, want empty (client referencing port in URL must not steal server's port)", aclient.Ports)
	}
	server := svcByName(af, "web/web/server")
	if server == nil || len(server.Ports) != 1 {
		t.Fatalf("server.Ports = %v, want 1 port", server)
	}
}

func TestCompile_doesNotLeavePickedPortsBound(t *testing.T) {
	af := compile(t, `
service "go" "api" {
  package = "example.com/api@v1.0.0"
  vars = { port = net::pickport() }
}
`, nil)
	api := svcByName(af, "api")
	if api == nil || len(api.Ports) != 1 {
		t.Fatalf("api.Ports = %v, want 1 port", api)
	}
	assertPortImmediatelyBindable(t, api.Ports[0])
}

func TestPlan_Compute_ambiguousUnownedPortLeftUnheld(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" {}`,
		"web/Alphasfile": `
package "web" {
  inputs = {
    port = { description = "Port.", type = number, default = net::pickport() }
  }

  component "web" {
    service "go" "svc_a" {
      package = "example.com/a@v1.0.0"
      vars = { port = inputs.port }
    }

    service "go" "svc_b" {
      package = "example.com/b@v1.0.0"
      vars = { port = inputs.port }
    }
  }
}
`,
	})
	af := openTree(t, root)
	for _, name := range []string{"web/web/svc_a", "web/web/svc_b"} {
		svc := svcByName(af, name)
		if svc == nil {
			t.Fatalf("service %s not found", name)
		}
		if len(svc.Ports) != 0 {
			t.Fatalf("%s.Ports = %v, want empty when unowned port is referenced by multiple services without readiness", name, svc.Ports)
		}
	}
}

func TestPlan_Compute_unownedPortWithoutReadinessNotClaimedByClientVar(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" {}`,
		"web/Alphasfile": `
package "web" {
  inputs = {
    port = { description = "Port.", type = number, default = net::pickport() }
  }

  component "web" {
    service "go" "server" {
      package = "example.com/server@v1.0.0"
      vars = { addr = "127.0.0.1:${inputs.port}" }
    }

    service "go" "aclient" {
      package = "example.com/aclient@v1.0.0"
      vars = { upstream_port = inputs.port }
    }
  }
}
`,
	})
	af := openTree(t, root)
	aclient := svcByName(af, "web/web/aclient")
	if aclient == nil {
		t.Fatal("service web/web/aclient not found")
	}
	if len(aclient.Ports) != 0 {
		t.Fatalf("aclient.Ports = %v, want empty (client with exact int var must not claim unowned port without readiness)", aclient.Ports)
	}
}

func TestResolve_readOnlyDoesNotCreatePortLocksOrTimeWait(t *testing.T) {
	home := t.TempDir()
	kc := zlock.NewGlobalKeychain(home)
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `
service "go" "a" {
  package = "example.com/a@v1.0.0"
  vars = { port = net::pickport() }
}

service "go" "b" {
  package = "example.com/b@v1.0.0"
  vars = { port = net::pickport() }
}
`,
	})
	tree, err := LoadTreeWith(root, LoadOptions{Keychain: kc})
	if err != nil {
		t.Fatalf("LoadTreeWith: %v", err)
	}

	for range 5 {
		af, err := Resolve(tree, testInv(), nil, testCfgHash, TestConfig{})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		for _, svc := range af.All() {
			for _, port := range svc.Ports {
				assertPortImmediatelyBindable(t, port)
			}
		}
	}

	entries, err := zfs.ReadDir(filepath.Join(home, "locks"))
	if err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "port-") {
				t.Fatalf("Resolve created unexpected port lockfile %s in %s/locks", e.Name(), home)
			}
		}
	}
}

func TestPlan_ReservePorts_nilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil.ReservePorts did not panic")
		}
	}()
	var p *Plan
	p.ReservePorts(zlock.GlobalKeychain{})
}

func assertPortImmediatelyBindable(t *testing.T, port int) {
	t.Helper()
	fd4, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socket(AF_INET): %v", err)
	}
	defer func() { _ = syscall.Close(fd4) }()
	sa4 := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	if err := syscall.Bind(fd4, sa4); err != nil {
		t.Fatalf("Bind(127.0.0.1:%d) with SO_REUSEADDR=0 failed: %v", port, err)
	}

	fd6, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0)
	if err == nil {
		defer func() { _ = syscall.Close(fd6) }()
		sa6 := &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
		if err := syscall.Bind(fd6, sa6); err != nil && !errors.Is(err, syscall.EAFNOSUPPORT) && !errors.Is(err, syscall.EADDRNOTAVAIL) {
			t.Fatalf("Bind([::1]:%d) with SO_REUSEADDR=0 failed (port left in TIME_WAIT): %v", port, err)
		}
	}
}
