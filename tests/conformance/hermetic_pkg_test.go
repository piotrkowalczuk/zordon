//go:build conformance_pkg

package conformance_test

import (
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// A pkg service's `mise install` goes through the same closed world as a
// language toolchain: a poisoned proxy would sink the aqua download of
// etcd, and the provision that runs alongside the package must see none
// of the host poison (its env is dumped to a file, since etcd cannot echo
// its environment back).
func TestPkgService_hermetic_hostEnvIgnored(t *testing.T) {
	home := poisonedHome(t, homeFile{".asdfrc", "legacy_version_file = yes\n"})

	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]

service "pkg" "etcd" {
  package = { name = "etcd-io/etcd", backend = "aqua", version = "3.5.17" }

  vars = {
    client = net::pickport()
    peer   = net::pickport()
    data   = "${fs::tmp()}/etcd-data"
    dump   = "${fs::state()}/etcd-env.txt"
  }

  runtime {
    after = [self.runtime.provision.dump.success]

    provision "dump" {
      cmd = "env > ${self.vars.dump}"
    }

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

	startPoisoned(t, p, home, map[string]string{
		"HTTPS_PROXY":     "http://127.0.0.1:1" + poisonSentinel,
		"HTTP_PROXY":      "http://127.0.0.1:1" + poisonSentinel,
		"ASDF_DATA_DIR":   poisonSentinel,
		"PKG_CONFIG_PATH": poisonSentinel,
		"CFLAGS":          "-poison",
	})

	dump := p.Get(t, "service.pkg.etcd.vars.dump").String()
	body, err := readFile(t, dump)
	if err != nil {
		t.Fatalf("provision env dump: %v", err)
	}
	assertNoPoison(t, envFromDump(body), "HTTPS_PROXY", "HTTP_PROXY", "ASDF_DATA_DIR", "PKG_CONFIG_PATH", "CFLAGS")
}
