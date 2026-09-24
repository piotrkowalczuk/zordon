// Module conformance: `module "<m>" {}` blocks give services a namespace
// (`module.<m>.service.<tc>.<n>`, display name `<m>/<n>`) and their own
// toolchain pin. `zordon plan` is the static oracle here: no alpha, no
// build, the rendered HCL must nest each module's services and pin under
// its block with every cross-module reference substituted.
package conformance_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestPlan_twoModulesTwoGoPins(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

toolchain {
  go { version = "1.27.0" }
}

service "go" "gateway" {
  package = "example.com/gateway@v0.0.0"
  vars = {
    port     = net::pickport()
    payments = module.payments.service.go.api.vars.port
    auth     = module.auth.service.go.api.vars.port
  }
  runtime { after = [module.payments.service.go.api.runtime.ready] }
}

module "payments" {
  toolchain {
    go { version = "1.22.0" }
  }

  service "go" "db" {
    package = "example.com/db@v0.0.0"
    vars = { port = net::pickport() }
  }
  service "go" "api" {
    package = "example.com/api@v0.0.0"
    vars = {
      port = net::pickport()
      db   = "127.0.0.1:${service.go.db.vars.port}"
    }
    runtime { after = [service.go.db.runtime.ready, toolchain.go.ready] }
  }
}

module "auth" {
  service "go" "db" {
    package = "example.com/db@v0.0.0"
    vars = { port = net::pickport() }
  }
  service "go" "api" {
    package = "example.com/api@v0.0.0"
    vars = {
      port = net::pickport()
      db   = "127.0.0.1:${service.go.db.vars.port}"
    }
    runtime { after = [toolchain.go.ready] }
  }
}
`)

	res := p.Zordon("plan").Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon plan: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	out := res.Stdout

	// Barrier refs keep their canonical `module.<m>.service…@state` form, so
	// only value traversals are forbidden here.
	for _, forbidden := range []string{"${", "self.", "module.payments.service.go.api.vars", "module.auth.service.go.api.vars", ".vars.port", "net::"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("unresolved token %q in\n%s", forbidden, out)
		}
	}
	for _, want := range []string{
		`module "payments" {`,
		`module "auth" {`,
		`version = "1.27.0"`,
		`version = "1.22.0"`,
		`"toolchain.payments/go@ready"`,
		`"toolchain.go@ready"`,
		`"module.payments.service.go.db.runtime@ready"`,
		`"module.payments.service.go.api.runtime@ready"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}

	// Two `service "go" "db"` blocks coexist, one per module, each with its
	// own concrete port; both api blocks point at their own module's db.
	if n := strings.Count(out, `service "go" "db"`); n != 2 {
		t.Errorf("want 2 db services, got %d\n%s", n, out)
	}
	portRE := regexp.MustCompile(`(?m)^\s*port\s*=\s*(\d+)\s*$`)
	if got := len(portRE.FindAllStringSubmatch(out, -1)); got != 5 {
		t.Errorf("want 5 concrete ports (gateway + 2 db + 2 api), got %d\n%s", got, out)
	}
}

func TestPlan_bareServiceRefDoesNotCrossModules(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

module "infra" {
  service "go" "db" {
    package = "example.com/db@v0.0.0"
    vars = { port = net::pickport() }
  }
}
module "apps" {
  service "go" "api" {
    package = "example.com/api@v0.0.0"
    vars = { db = service.go.db.vars.port }
  }
}
`)
	res := p.Zordon("plan").Run(t)
	if res.ExitCode == 0 {
		t.Fatalf("plan must fail: module apps has no db, and bare service.* never reaches module infra\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "db") {
		t.Errorf("error should name the missing service, got:\n%s", res.Stderr)
	}
}
