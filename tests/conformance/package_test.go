// Package conformance, driven through `zordon plan` (static, no alpha): a
// package's features switch its services on and off, its inputs reach the
// rendered config, entries imports add to a map input reach its package,
// and an identity import resolves through zordon.work.
package conformance_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

const pkgGreeter = `
package "greeter" {
  inputs   = { greeting = { description = "How to greet.", type = string, default = "hello" } }
  features = { extra = { description = "Adds the extra service" } }

  module "greeter" {
    service "go" "greeter" {
      package = "example.com/greeter@v0.0.0"
      vars    = { greeting = inputs.greeting }
    }

    service "go" "extra" {
      enabled = features.extra
      package = "example.com/extra@v0.0.0"
    }
  }
}
`

func TestPlan_packageFeaturesChangeTheStack(t *testing.T) {
	cases := map[string]struct {
		entry string
		extra bool
	}{
		"off": {entry: `import "./greeter" {}`},
		"on":  {entry: `import "./greeter" { features = ["extra"] }`, extra: true},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			p := zordontest.NewProject(t)
			p.WriteFile("Alphasfile", c.entry)
			p.WriteFile("greeter/Alphasfile", pkgGreeter)
			out := planOK(t, p)
			if !strings.Contains(out, `service "go" "greeter"`) {
				t.Errorf("greeter missing:\n%s", out)
			}
			if got := strings.Contains(out, `service "go" "extra"`); got != c.extra {
				t.Errorf("extra rendered = %v, want %v:\n%s", got, c.extra, out)
			}
		})
	}
}

func TestPlan_mapInputEntriesReachThePackage(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", "import \"./shop\" {}\nimport \"./blog\" {}\n")
	p.WriteFile("proxy/Alphasfile", `
package "proxy" {
  inputs = { sites = { description = "Hosts to route.", type = map(object({ host = string, port = number })), default = {} } }

  module "proxy" {
    service "go" "proxy" {
      package = "example.com/proxy@v0.0.0"
      file "routes" {
        path = "/tmp/routes"
        body = "%{for name, s in inputs.sites}${s.host}=${s.port};%{endfor}"
      }
    }
  }
}
`)
	for name, port := range map[string]string{"shop": "8081", "blog": "8082"} {
		p.WriteFile(name+"/Alphasfile", `
package "`+name+`" {
  import "../proxy" {
    inputs = {
      sites = {
        `+name+` = {
          host = "`+name+`.test"
          port = module.site.service.go.site.vars.port
        }
      }
    }
  }

  module "site" {
    service "go" "site" {
      package = "example.com/site@v0.0.0"
      vars    = { port = `+port+` }
    }
  }
}
`)
	}
	if out := planOK(t, p); !strings.Contains(out, `body = "blog.test=8082;shop.test=8081;"`) {
		t.Errorf("sites added by the importers missing from the package that takes them:\n%s", out)
	}
}

func TestPlan_packageInputReachesConfig(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", `import "./greeter" { inputs = { greeting = "hi from input" } }`)
	p.WriteFile("greeter/Alphasfile", pkgGreeter)
	if out := planOK(t, p); !strings.Contains(out, `greeting = "hi from input"`) {
		t.Errorf("input value missing from the rendered stack:\n%s", out)
	}
}

func TestPlan_identityImportThroughZordonWork(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("checkouts/infra/zordon.mod", `module = "github.com/acme/infra"`)
	p.WriteFile("checkouts/infra/pkgs/greeter/Alphasfile", pkgGreeter)
	p.WriteFile("zordon.work", `search "./checkouts/infra" {}`)
	p.WriteFile("Alphasfile", "require \"github.com/acme/infra\" { ref = \"main\" }\nimport \"github.com/acme/infra/pkgs/greeter\" {}\n")
	out := planOK(t, p)
	dir, err := filepath.EvalSymlinks(p.Dir())
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(dir, "checkouts", "infra")
	want := "# import " + filepath.Join(checkout, "pkgs", "greeter") + " as greeter (search " + checkout + ")"
	if !strings.Contains(out, want) {
		t.Errorf("missing %q in\n%s", want, out)
	}
}

func TestPkgGetAndUpdate(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("checkouts/infra/zordon.mod", `module = "github.com/acme/infra"`)
	p.WriteFile("checkouts/infra/pkgs/greeter/Alphasfile", pkgGreeter)
	p.WriteFile("zordon.work", `search "./checkouts/infra" {}`)
	p.WriteFile("Alphasfile", "import \"github.com/acme/infra/pkgs/greeter\" {}\n")

	if res := p.Zordon("plan").Run(t); res.ExitCode == 0 || !strings.Contains(res.Stderr, "run zordon pkg get github.com/acme/infra@<ref>") {
		t.Fatalf("plan without a require: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	res := p.Zordon("pkg", "get", "github.com/acme/infra@main").Run(t)
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, `require "github.com/acme/infra" { ref = "main" }`) {
		t.Fatalf("pkg get: exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	planOK(t, p)
	if res := p.Zordon("pkg", "update").Run(t); res.ExitCode != 0 || !strings.Contains(res.Stdout, "zordon.lock is up to date") {
		t.Errorf("pkg update: exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if res := p.Zordon("pkg", "get").Run(t); res.ExitCode == 0 || !strings.Contains(res.Stderr, "usage: zordon pkg get <repo>@<ref>") {
		t.Errorf("pkg get without an argument: exit %d\n%s", res.ExitCode, res.Stderr)
	}
}

func TestGet_packageService(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", `import "./greeter" { inputs = { greeting = "hi" } }`)
	p.WriteFile("greeter/Alphasfile", pkgGreeter)
	res := p.Zordon("get", "package.greeter.module.greeter.service.go.greeter.vars.greeting").Run(t)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hi" {
		t.Fatalf("zordon get: exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
}

func TestPlan_twoZordonWorkFilesFail(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("zordon.work", "")
	p.WriteFile("app/zordon.work", "")
	p.WriteFile("app/Alphasfile", "service \"go\" \"a\" {\n  package = \"example.com/a@v0.0.0\"\n}\n")
	res := p.Zordon("plan").WithDir("app").Run(t)
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "found 2 zordon.work files above") {
		t.Fatalf("exit %d, stderr:\n%s", res.ExitCode, res.Stderr)
	}
}

func TestPlan_picksAPackageOrAModule(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", "import \"./shop\" {}\nimport \"./billing\" {}\n")
	p.WriteFile("shop/Alphasfile", `
package "shop" {
  module "shop" {
    service "go" "storefront" { package = "example.com/storefront@v0.0.0" }
  }
  module "admin" {
    service "go" "panel" { package = "example.com/panel@v0.0.0" }
  }
}
`)
	p.WriteFile("billing/Alphasfile", "package \"billing\" {\n  module \"billing\" {\n    service \"go\" \"invoicer\" { package = \"example.com/invoicer@v0.0.0\" }\n  }\n}\n")
	cases := map[string]struct{ pick, in, out string }{
		"package":             {pick: "shop", in: `service "go" "storefront"`, out: `service "go" "invoicer"`},
		"module of a package": {pick: "shop/admin", in: `service "go" "panel"`, out: `service "go" "storefront"`},
		"unique service":      {pick: "invoicer", in: `service "go" "invoicer"`, out: `service "go" "panel"`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			res := p.Zordon("plan", c.pick).Run(t)
			if res.ExitCode != 0 {
				t.Fatalf("zordon plan %s: exit %d\n%s", c.pick, res.ExitCode, res.Stderr)
			}
			if !strings.Contains(res.Stdout, c.in) || strings.Contains(res.Stdout, c.out) {
				t.Errorf("zordon plan %s must render %s and leave out %s:\n%s", c.pick, c.in, c.out, res.Stdout)
			}
		})
	}
}

func planOK(t *testing.T, p *zordontest.Project) string {
	t.Helper()
	res := p.Zordon("plan").Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon plan: exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	return res.Stdout
}
