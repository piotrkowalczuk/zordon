// Start-summary conformance: `zordon start --summary` prints, after a
// successful bringup, a timing block listing every service's bringup phases
// (wait / build / spawn / ready / total) in start order plus each provision's
// run — and without the flag that block stays hidden.
package conformance_test

import (
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

const summaryRule = "------------------------------------------------------------"

// Two services where api depends on db, plus a provision on api, so the
// summary has something to order (db before api), a real dep to report, and a
// provisions section.
func TestStartSummary_printedWithFlag(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/db")
	p.CopyTree("golden/go/echo", "src/api")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "db" {
  src {
    path = "./src/db"
    exe  = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/db", "-addr", "127.0.0.1:${self.vars.port}"]
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

service "go" "api" {
  src {
    path = "./src/api"
    exe  = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd   = ["${fs::bin()}/api", "-addr", "127.0.0.1:${self.vars.port}"]
    after = [service.go.db.runtime.ready]

    provision "seed" {
      cmd = "echo seeded"
    }
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

	t.Cleanup(func() { dumpAlphaLog(t, p.AlphaLogPath()) })

	res := p.Zordon("start", "--summary",
		"--timeout", "240s",
		"--alpha-log", p.AlphaLogPath(),
	).WithTimeout(245 * time.Second).Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon start --summary: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	combined := res.Stdout + res.Stderr

	i := strings.Index(combined, summaryRule)
	if i < 0 {
		t.Fatalf("no summary opening rule in output\nstdout: %s\nstderr: %s", res.Stdout, res.Stderr)
	}
	j := strings.Index(combined[i+len(summaryRule):], summaryRule)
	if j < 0 {
		t.Fatalf("no summary closing rule; output truncated?\n%s", combined[i:])
	}
	block := combined[i : i+len(summaryRule)+j+len(summaryRule)]

	mustContain := []string{
		"Bringup complete: 2 service(s), 1 provision(s)",
		"wait", "build", "spawn", "ready", "total", // phase columns
		"db",
		"api",
		"service.go.db", // api's dep, whatever exact ref form resolves to
		"provisions:",
		"seed",
	}
	for _, want := range mustContain {
		if !strings.Contains(block, want) {
			t.Errorf("summary block missing %q\n----- block -----\n%s\n-----", want, block)
		}
	}
}

// Without --summary (and without -v), a successful start prints no summary
// block — only the plain "alpha ready" line.
func TestStartSummary_hiddenWithoutFlag(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc" {
  src {
    path = "./src/svc"
    exe  = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc", "-addr", "127.0.0.1:${self.vars.port}"]
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

	t.Cleanup(func() { dumpAlphaLog(t, p.AlphaLogPath()) })

	res := p.Zordon("start",
		"--timeout", "240s",
		"--alpha-log", p.AlphaLogPath(),
	).WithTimeout(245 * time.Second).Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon start: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	combined := res.Stdout + res.Stderr
	if strings.Contains(combined, "Bringup complete") {
		t.Errorf("summary printed without --summary\n%s", combined)
	}
}
