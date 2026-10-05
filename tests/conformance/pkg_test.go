//go:build conformance_pkg

package conformance_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// TestPkgService_etcd_tcpReadiness brings up a real native package via
// mise (etcd's own GitHub release through the ubi backend — a prebuilt
// single binary, so no source compile), runs it as a supervised process,
// and asserts it reaches ready via the TCP probe and actually serves.
// End-to-end proof that pkg services install + run + supervise like any
// other service.
//
// Cost: first run downloads etcd via mise/ubi (network). Subsequent runs
// reuse <repo>/.zordon. ubi rather than aqua: aqua's etcd registry entry
// pins darwin to the amd64 build (rosetta2), which a Mac without Rosetta
// cannot exec, while ubi picks the release's native darwin-arm64 asset.
func TestPkgService_etcd_tcpReadiness(t *testing.T) {
	p := zordontest.NewProject(t)

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]

service "pkg" "etcd" {
  package = { name = "etcd-io/etcd[exe=etcd]", backend = "ubi", version = "3.6.15" }

  vars = {
    client = net::pickport()
    peer   = net::pickport()
    data   = "${fs::tmp()}/etcd-data"
  }

  runtime {
    cmd = [
      "etcd",
      "--name", "zordon-etcd",
      "--data-dir", "${self.vars.data}",
      "--listen-client-urls", "http://127.0.0.1:${self.vars.client}",
      "--advertise-client-urls", "http://127.0.0.1:${self.vars.client}",
      "--listen-peer-urls", "http://127.0.0.1:${self.vars.peer}",
      "--initial-advertise-peer-urls", "http://127.0.0.1:${self.vars.peer}",
      "--initial-cluster", "zordon-etcd=http://127.0.0.1:${self.vars.peer}",
    ]
  }

  readiness {
    tcp { port = self.vars.client }
    period            = "200ms"
    failure_threshold = 150
  }
}
`)

	mustStart(t, p)

	// Readiness already proved the client port accepts TCP. Confirm it's
	// really etcd by hitting its HTTP /version endpoint on that port.
	port := p.Get(t, "service.pkg.etcd.vars.client").String()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/version", port))
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "etcdserver") {
		t.Fatalf("unexpected /version response: status=%d body=%s", resp.StatusCode, body)
	}
}
