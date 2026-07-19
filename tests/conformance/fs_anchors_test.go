//go:build conformance_go

// fs_anchors conformance pins the #46 fix end-to-end: a file{} anchored at
// fs::etc() is materialized on disk under the persistent per-workspace state
// dir (<state>/etc/<svc>), not under the OS per-invocation temp dir
// ($TMPDIR/zordon-<hash>) that the macOS janitor reaps after ~3 idle days.
package conformance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestFsAnchors_etcVar_materializeUnderStateDir(t *testing.T) {
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

  vars = { port = net::pickport(), data = "${fs::var()}/state" }

  file "conf" {
    path = "${fs::etc()}/svc1.conf"
    body = "port=${self.vars.port}\n"
  }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
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
		t.Fatalf("zordon start: exit %d\nstdout: %s\nstderr: %s", start.ExitCode, start.Stdout, start.Stderr)
	}

	confPath := p.Get(t, "service.go.svc1.file.conf.path").String()
	// fs::etc() → <root>/workspaces/main/etc/svc1/svc1.conf — durable, in the
	// state dir, NOT under the reap-prone $TMPDIR/zordon-<hash> scratch dir.
	if !strings.Contains(confPath, filepath.Join("workspaces", "main", "etc", "svc1")) {
		t.Errorf("fs::etc() path = %q, want it under workspaces/main/etc/svc1", confPath)
	}
	if strings.Contains(confPath, "zordon-") {
		t.Errorf("durable config must not live in the per-invocation tmp dir: %q", confPath)
	}
	if _, err := os.Stat(confPath); err != nil {
		t.Errorf("declared file{} not materialized on disk at %q: %v", confPath, err)
	}

	dataDir := p.Get(t, "service.go.svc1.vars.data").String()
	if !strings.Contains(dataDir, filepath.Join("workspaces", "main", "var", "svc1")) {
		t.Errorf("fs::var() path = %q, want it under workspaces/main/var/svc1", dataDir)
	}
}
