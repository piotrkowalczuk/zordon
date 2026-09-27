package alphasfile

import (
	"maps"
	"strings"
	"testing"
)

const pkgGated = `
package "web" {
  features = { extra = "Adds the extra blocks" }

  module "web" {
    service "go" "web" {
      git { url = "github.com/x/web" }

      file "base" {
        path = "/tmp/base"
        body = "base"
      }

      file "extra" {
        enabled = features.extra
        path    = "/tmp/extra"
        body    = "extra"
      }

      runtime {
        provision "always" {
          after = never
          cmd   = "true"
        }
        provision "extra" {
          enabled = features.extra
          after   = never
          cmd     = "true"
        }
      }

      sudo "extra" {
        enabled = features.extra
        apply   = "true"
      }
    }

    service "go" "sidecar" {
      enabled = features.extra
      git { url = "github.com/x/sidecar" }
    }
  }
}
`

func TestOpen_featureOffRemovesGatedBlocks(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" {}`,
		"web/Alphasfile": pkgGated,
	})
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"web/web/web"}) {
		t.Fatalf("services = %v", got)
	}
	web := svcByName(af, "web/web/web")
	if files := fileNames(web); !equalStrs(files, []string{"base"}) {
		t.Errorf("files = %v", files)
	}
	if provByName(web, "extra") != nil || provByName(web, "always") == nil {
		t.Errorf("provisions = %+v", web.Runtime.Provision)
	}
	if len(web.Runtime.Sudo) != 0 {
		t.Errorf("sudo = %+v", web.Runtime.Sudo)
	}
}

func TestOpen_featureOnKeepsGatedBlocks(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" { features = ["extra"] }`,
		"web/Alphasfile": pkgGated,
	})
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"web/web/web", "web/web/sidecar"}) {
		t.Fatalf("services = %v", got)
	}
	web := svcByName(af, "web/web/web")
	if files := fileNames(web); !equalStrs(files, []string{"base", "extra"}) {
		t.Errorf("files = %v", files)
	}
	if provByName(web, "extra") == nil || len(web.Runtime.Sudo) != 1 {
		t.Errorf("provisions = %+v, sudo = %+v", web.Runtime.Provision, web.Runtime.Sudo)
	}
}

func TestOpen_featureGatesRequire(t *testing.T) {
	files := map[string]string{
		"web/Alphasfile": `
package "web" {
  features = { metrics = "Scrapes metrics with prom" }

  import "../prom" {
    enabled = features.metrics
  }

  module "web" {
    service "go" "web" {
      git { url = "github.com/x/web" }

      file "scrape" {
        enabled = features.metrics
        path    = "/tmp/scrape"
        body    = "port=${package.prom.module.prom.service.go.prom.vars.port}"
      }
    }
  }
}
`,
		"prom/Alphasfile": `
package "prom" {
  module "prom" {
    service "go" "prom" {
      git { url = "github.com/x/prom" }
      vars = { port = 9090 }
    }
  }
}
`,
	}
	cases := map[string]struct {
		entry    string
		services []string
	}{
		"off": {`import "./web" {}`, []string{"web/web/web"}},
		"on":  {`import "./web" { features = ["metrics"] }`, []string{"web/web/web", "prom/prom/prom"}},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			tree := map[string]string{"Alphasfile": c.entry}
			maps.Copy(tree, files)
			af := openTree(t, writeTree(t, t.TempDir(), tree))
			if got := serviceNames(af); !equalStrs(got, c.services) {
				t.Errorf("services = %v, want %v", got, c.services)
			}
		})
	}
}

func TestLoadTree_referenceToSwitchedOffBlock(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" {}`,
		"web/Alphasfile": `
package "web" {
  features = { extra = "Adds the extra blocks" }

  module "web" {
    service "go" "sidecar" {
      enabled = features.extra
      git { url = "github.com/x/sidecar" }
      vars = { port = 1 }
    }

    service "go" "web" {
      git { url = "github.com/x/web" }
      vars = { peer = service.go.sidecar.vars.port }
    }
  }
}
`,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), `references service "go" "sidecar", which is switched off by enabled at`) || !strings.Contains(err.Error(), "give this block the same enabled") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_referenceToSwitchedOffRequire(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" {}`,
		"web/Alphasfile": `
package "web" {
  features = { metrics = "Scrapes metrics with prom" }

  import "../prom" {
    enabled = features.metrics
  }

  module "web" {
    service "go" "web" {
      git { url = "github.com/x/web" }
      vars = { prom = package.prom.module.prom.service.go.prom.name }
    }
  }
}
`,
		"prom/Alphasfile": "package \"prom\" {\n  module \"prom\" {\n    service \"go\" \"prom\" {\n      git { url = \"github.com/x/prom\" }\n    }\n  }\n}\n",
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), "references package.prom, which is switched off by enabled at") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_enabledAcceptsFeatureExpressionsOnly(t *testing.T) {
	cases := map[string]struct{ expr, want string }{
		"function call":   {`upper("x") == "X"`, "cannot call functions"},
		"other root":      {`inputs.flag`, "enabled may reference features.<name> only"},
		"unknown feature": {`features.nope`, `unknown feature "nope" (declared: extra)`},
		"not a bool":      {`"yes"`, "enabled must be true or false"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile": `import "./web" {}`,
				"web/Alphasfile": "package \"web\" {\n  features = { extra = \"Adds the extra blocks\" }\n  inputs = { flag = true }\n  module \"web\" {\n" +
					"    service \"go\" \"web\" {\n      enabled = " + c.expr + "\n      git { url = \"github.com/x/web\" }\n    }\n  }\n}\n",
			})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_enabledCombinesFeatures(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" { features = ["a"] }`,
		"web/Alphasfile": `
package "web" {
  features = { a = "Turns on a", b = "Turns on b" }

  module "web" {
    service "go" "both" {
      enabled = features.a && features.b
      git { url = "github.com/x/both" }
    }

    service "go" "either" {
      enabled = features.a || features.b
      git { url = "github.com/x/either" }
    }

    service "go" "not-b" {
      enabled = !features.b
      git { url = "github.com/x/notb" }
    }
  }
}
`,
	})
	if got := serviceNames(openTree(t, root)); !equalStrs(got, []string{"web/web/either", "web/web/not-b"}) {
		t.Errorf("services = %v", got)
	}
}

func TestLoadTree_unknownFeatureInImport(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" { features = ["nope"] }`,
		"web/Alphasfile": pkgGated,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `feature "nope" is not declared by package web (declared: extra)`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_enabledNeedsAPackage(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./Alphasfile.f" { modules = ["m"] }`,
		"Alphasfile.f": `
module "m" {
  service "go" "web" {
    enabled = true
    git { url = "github.com/x/web" }
  }
}
`,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "enabled needs a feature, and none is declared here") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_enabledOnEntrypointImportIsRejected(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "import \"./web\" {\n  enabled = true\n}\n",
		"web/Alphasfile": pkgWeb,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `import "./web": enabled needs a feature, and none is declared here; only a package declares features = { ... }`) {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_featureGatesImportOfAPackageRunOnItsOwn(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "package \"stack\" {\n  features = { x = \"Turns on x\" }\n  import \"./web\" {\n    enabled = features.x\n  }\n}\n",
		"web/Alphasfile": pkgWeb,
	})
	if got := serviceNames(openTree(t, root)); len(got) != 0 {
		t.Errorf("services = %v; feature x is off when the package runs on its own", got)
	}
}

func TestLoadTree_featuresAreDescribed(t *testing.T) {
	cases := map[string]struct{ features, want string }{
		"list":           {`["a"]`, `features of package "web" map each name to what it turns on, such as { tls = "Serves HTTPS with a local CA" }`},
		"invalid name":   {`{ "no spaces" = "x" }`, `feature "no spaces" of package "web": use letters`},
		"no description": {`{ a = "" }`, `feature "a" of package "web" needs a description of what it turns on`},
		"not a string":   {`{ a = true }`, `feature "a" of package "web" needs a description`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":     `import "./web" {}`,
				"web/Alphasfile": "package \"web\" {\n  features = " + c.features + "\n}\n",
			})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func fileNames(s *Service) []string {
	var out []string
	for _, f := range s.Runtime.Files {
		out = append(out, f.Name)
	}
	return out
}
