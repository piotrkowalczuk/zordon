package alphasfile

import (
	"slices"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zlock"
	"github.com/piotrkowalczuk/zordon/internal/znet"
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

	plan.r.pickedPorts = make(map[int]bool, znet.PortRangeSpan)
	for i := range znet.PortRangeSpan {
		p := znet.PortRangeBase + i
		if p != wsPort {
			plan.r.pickedPorts[p] = true
		}
	}

	if _, err := plan.Compute(); err == nil || !strings.Contains(err.Error(), "no free port available") {
		t.Fatalf("Compute when only WorkspacePort(%q)=%d is unpicked: err = %v, want no free port available", inv.FsHash, wsPort, err)
	}
}

func TestPlan_Compute_respectsGlobalKeychainPortLocks(t *testing.T) {
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
	m.tree.kc = kc
	plan, err := m.Plan(nil, testCfgHash, TestConfig{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	af, err := plan.Compute()
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	api := svcByName(af, "api")
	if api == nil || len(api.Ports) != 1 {
		t.Fatalf("api.Ports = %v, want 1 port", api.Ports)
	}
	port := api.Ports[0]

	unlock, err := kc.TryLockPort(port)
	if err != nil {
		t.Fatalf("TryLockPort(%d) after Compute: %v", port, err)
	}
	unlock()
}

func TestPlan_Compute_claimsUnownedPortFromPackageInput(t *testing.T) {
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
	if len(svc.Ports) != 2 {
		t.Fatalf("svc.Ports = %v, want 2 ports (http_port from vars + admin_port from readiness)", svc.Ports)
	}
}
