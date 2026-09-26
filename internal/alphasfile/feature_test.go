package alphasfile

import (
	"maps"
	"strings"
	"testing"
)

const pkgGated = `
feature "extra" {}

service "go" "web" {
  git { url = "github.com/x/web" }

  file "base" {
    path = "/tmp/base"
    body = "base"
  }

  file "extra" {
    enabled = feature.extra
    path    = "/tmp/extra"
    body    = "extra"
  }

  runtime {
    provision "always" {
      after = never
      cmd   = "true"
    }
    provision "extra" {
      enabled = feature.extra
      after   = never
      cmd     = "true"
    }
  }
}

service "go" "sidecar" {
  enabled = feature.extra
  git { url = "github.com/x/sidecar" }
}
`

func TestOpen_featureOffRemovesGatedBlocks(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" {}`,
		"web/Alphasfile": pkgGated,
	})
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"web/web"}) {
		t.Fatalf("services = %v", got)
	}
	web := svcByName(af, "web/web")
	if files := fileNames(web); !equalStrs(files, []string{"base"}) {
		t.Errorf("files = %v", files)
	}
	if provByName(web, "extra") != nil || provByName(web, "always") == nil {
		t.Errorf("provisions = %+v", web.Runtime.Provision)
	}
}

func TestOpen_featureOnKeepsGatedBlocks(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" { features = ["extra"] }`,
		"web/Alphasfile": pkgGated,
	})
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"web/web", "web/sidecar"}) {
		t.Fatalf("services = %v", got)
	}
	web := svcByName(af, "web/web")
	if files := fileNames(web); !equalStrs(files, []string{"base", "extra"}) {
		t.Errorf("files = %v", files)
	}
	if provByName(web, "extra") == nil {
		t.Errorf("provision extra missing: %+v", web.Runtime.Provision)
	}
}

func TestOpen_featureGatesRequire(t *testing.T) {
	files := map[string]string{
		"web/Alphasfile": `
feature "metrics" {}

require "../prom" {
  enabled = feature.metrics
}

service "go" "web" {
  git { url = "github.com/x/web" }

  file "scrape" {
    enabled = feature.metrics
    path    = "/tmp/scrape"
    body    = "port=${module.prom.service.go.prom.vars.port}"
  }
}
`,
		"prom/Alphasfile": `
service "go" "prom" {
  git { url = "github.com/x/prom" }
  vars = { port = 9090 }
}
`,
	}
	cases := map[string]struct {
		entry    string
		services []string
	}{
		"off": {`import "./web" {}`, []string{"web/web"}},
		"on":  {`import "./web" { features = ["metrics"] }`, []string{"web/web", "prom/prom"}},
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
feature "extra" {}

service "go" "sidecar" {
  enabled = feature.extra
  git { url = "github.com/x/sidecar" }
  vars = { port = 1 }
}

service "go" "web" {
  git { url = "github.com/x/web" }
  vars = { peer = service.go.sidecar.vars.port }
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
feature "metrics" {}

require "../prom" {
  enabled = feature.metrics
}

service "go" "web" {
  git { url = "github.com/x/web" }
  vars = { prom = module.prom.service.go.prom.name }
}
`,
		"prom/Alphasfile": "service \"go\" \"prom\" {\n  git { url = \"github.com/x/prom\" }\n}\n",
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), "references module.prom, which is switched off by enabled at") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_enabledAcceptsFeatureExpressionsOnly(t *testing.T) {
	cases := map[string]struct{ expr, want string }{
		"function call":   {`upper("x") == "X"`, "cannot call functions"},
		"other root":      {`input.flag`, "enabled may reference feature.<name> only"},
		"unknown feature": {`feature.nope`, `unknown feature "nope" (declared: extra)`},
		"not a bool":      {`"yes"`, "enabled must be true or false"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile": `import "./web" {}`,
				"web/Alphasfile": "feature \"extra\" {}\ninput \"flag\" { default = true }\n" +
					"service \"go\" \"web\" {\n  enabled = " + c.expr + "\n  git { url = \"github.com/x/web\" }\n}\n",
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
feature "a" {}
feature "b" {}

service "go" "both" {
  enabled = feature.a && feature.b
  git { url = "github.com/x/both" }
}

service "go" "either" {
  enabled = feature.a || feature.b
  git { url = "github.com/x/either" }
}

service "go" "not-b" {
  enabled = !feature.b
  git { url = "github.com/x/notb" }
}
`,
	})
	if got := serviceNames(openTree(t, root)); !equalStrs(got, []string{"web/either", "web/not-b"}) {
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

func TestLoadTree_enabledNeedsDeclaredFeature(t *testing.T) {
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

func TestLoadTree_enabledOnImportIsRejected(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "feature \"x\" {}\nimport \"./web\" {\n  enabled = feature.x\n}\n",
		"web/Alphasfile": pkgWeb,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "enabled is allowed on require only") {
		t.Fatalf("got %v", err)
	}
}

func fileNames(s *Service) []string {
	var out []string
	for _, f := range s.Runtime.Files {
		out = append(out, f.Name)
	}
	return out
}
