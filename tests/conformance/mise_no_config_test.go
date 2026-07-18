// A stray mise.toml next to the Alphasfile (e.g. left behind by `mise use`)
// must not influence toolchain resolution: zordon runs every mise command with
// --no-config. The poison here is a far-future min_version, which makes any
// mise invocation that READS the config abort the version gate — so if
// --no-config ever regresses, bringup fails and this test goes red. Passing
// also proves --no-config does not break the real `mise install go@...` path.
package conformance_test

import (
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestMiseNoConfig_strayMiseTomlIgnored(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	// Sitting next to the Alphasfile — alpha's cwd — this would abort every
	// mise command if discovered.
	p.WriteFile("mise.toml", "min_version = \"9999.0.0\"\n")

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
		t.Fatalf("zordon start failed — stray mise.toml was not ignored?\nexit %d\nstdout: %s\nstderr: %s",
			start.ExitCode, start.Stdout, start.Stderr)
	}
}
