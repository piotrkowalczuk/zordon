//go:build conformance_go

// zordon pre-creates each service's fs::etc / fs::var dir at bringup, so a
// provision or cmd can write into ${fs::var()}/… without a manual mkdir -p.
// Without pre-creation the shell redirect below fails, the provision fails, and
// failfast aborts the bringup — so a green start proves the dir existed.
package conformance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestAnchorDirs_provisionWritesVarWithoutMkdir(t *testing.T) {
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

    provision "seed" {
      cmd = "echo ok > ${fs::var()}/marker.txt"
    }
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 100
  }
}
`)

	start := p.Zordon("start",
		"--timeout", "120s",
		"--alpha-log", p.AlphaLogPath(),
	).WithTimeout(125 * time.Second).Run(t)
	if start.ExitCode != 0 {
		t.Fatalf("zordon start failed — was the fs::var() dir pre-created?\nexit %d\nstdout: %s\nstderr: %s",
			start.ExitCode, start.Stdout, start.Stderr)
	}

	// <StateDir>/var/svc1/marker.txt, StateDir == <root>/workspaces/main.
	marker := filepath.Join(p.Dir(), "workspaces", "main", "var", "svc1", "marker.txt")
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("provision output not found at %s: %v", marker, err)
	}
	if !strings.Contains(string(b), "ok") {
		t.Errorf("marker = %q, want it to contain ok", string(b))
	}
}
