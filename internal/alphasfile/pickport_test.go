package alphasfile

import (
	"slices"
	"testing"

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
