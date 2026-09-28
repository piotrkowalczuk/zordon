package alphasfile

import (
	"fmt"
	"strings"
	"testing"
)

const pkgProxy = `
package "proxy" {
  inputs = {
    sites = {
      description = "Hosts to route, by site."
      type        = map(object({ host = string, port = number, tls = optional(bool, false) }))
      default     = {}
      unique      = ["host"]
    }
  }

  component "proxy" {
    service "go" "proxy" {
      git { url = "github.com/x/proxy" }
      vars = { routes = "%{ for k, s in inputs.sites }${k}:${s.host}=${s.port}/${s.tls};%{ endfor }" }
    }
  }
}
`

// pkgSite is a site that registers itself with the proxy it imports.
func pkgSite(name string, port int) string {
	return fmt.Sprintf(`
package %[1]q {
  import "../proxy" {
    inputs = {
      sites = {
        %[1]s = {
          host = "%[1]s.test"
          port = component.site.service.go.site.vars.port
        }
      }
    }
  }

  component "site" {
    service "go" "site" {
      git { url = "github.com/x/site" }
      vars = { port = %[2]d }
    }
  }
}
`, name, port)
}

func TestOpen_mapInputJoinsEveryImport(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./shop\" {}\nimport \"./blog\" {}\n",
		"proxy/Alphasfile": pkgProxy,
		"shop/Alphasfile":  pkgSite("shop", 8081),
		"blog/Alphasfile":  pkgSite("blog", 8082),
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "blog:blog.test=8082/false;shop:shop.test=8081/false;" {
		t.Errorf("routes = %q; each site adds its entry, evaluated before the proxy reads it, with defaults applied", got)
	}
}

func TestOpen_entrypointAddsToAMapInput(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./shop\" {}\nimport \"./proxy\" {\n  inputs = { sites = { docs = { host = \"docs.test\", port = 9000, tls = true } } }\n}\n",
		"proxy/Alphasfile": pkgProxy,
		"shop/Alphasfile":  pkgSite("shop", 8081),
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "docs:docs.test=9000/true;shop:shop.test=8081/false;" {
		t.Errorf("routes = %q; the entrypoint adds entries like any other import", got)
	}
}

func TestOpen_mapInputTakesItsDefault(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       `import "./proxy" {}`,
		"proxy/Alphasfile": pkgProxy,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "" {
		t.Errorf("routes = %q; a map input nobody sets takes its default", got)
	}
}

func TestOpen_mapInputDefaultIsNotJoined(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./shop\" {}\n",
		"proxy/Alphasfile": strings.Replace(pkgProxy, "default     = {}", `default     = { base = { host = "base.test", port = 1 } }`, 1),
		"shop/Alphasfile":  pkgSite("shop", 8081),
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "shop:shop.test=8081/false;" {
		t.Errorf("routes = %q; the default is used only when no import sets the input", got)
	}
}

func TestOpen_switchedOffImportPassesNothing(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./proxy\" {}\nimport \"./shop\" {}\n",
		"proxy/Alphasfile": pkgProxy,
		"shop/Alphasfile": `
package "shop" {
  features = { public = { description = "Serves the shop through the proxy." } }

  import "../proxy" {
    enabled = features.public
    inputs  = { sites = { shop = { host = "shop.test", port = 1 } } }
  }
}
`,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "" {
		t.Errorf("routes = %q", got)
	}
}

func TestOpen_mapKeySetTwice(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./shop\" {}\nimport \"./proxy\" {\n  inputs = { sites = { shop = { host = \"old.test\", port = 1 } } }\n}\n",
		"proxy/Alphasfile": pkgProxy,
		"shop/Alphasfile":  pkgSite("shop", 8081),
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `input "sites" of package proxy: `) || !strings.Contains(err.Error(), "shop/Alphasfile:5") || !strings.Contains(err.Error(), `key "shop" is already set at `+root+":3") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_mapUniqueCollision(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./shop\" {}\nimport \"./proxy\" {\n  inputs = { sites = { legacy = { host = \"shop.test\", port = 1 } } }\n}\n",
		"proxy/Alphasfile": pkgProxy,
		"shop/Alphasfile":  pkgSite("shop", 8081),
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `entry "shop": host = "shop.test" is already used by entry "legacy" at `+root+":3") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_mapEntryErrors(t *testing.T) {
	cases := map[string]struct{ sites, want string }{
		"wrong type":           {`{ a = { host = "a", port = "not a number" } }`, `["a"].port: a number is required, got string`},
		"undeclared attribute": {`{ a = { hots = "a", port = 1 } }`, `["a"].hots: attribute not declared by object({ host = string, port = number, tls = optional(bool) })`},
		"missing attribute":    {`{ a = { host = "a" } }`, `["a"]: attribute "port" is required`},
		"not a map":            {`"a"`, "a map is required, got string"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":       "import \"./proxy\" {\n  inputs = { sites = " + c.sites + " }\n}\n",
				"proxy/Alphasfile": pkgProxy,
			})
			_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
			if err == nil || !strings.Contains(err.Error(), `input "sites" of package proxy: `+root+":2") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestOpen_inputRefErrors(t *testing.T) {
	cases := map[string]struct{ files map[string]string }{
		"unknown input": {map[string]string{
			"Alphasfile": `import "./p" {}`,
			"p/Alphasfile": `
package "p" {
  inputs = { sites = { description = "Sites.", type = map(object({ host = string })), default = {} } }
  component "m" {
    service "go" "s" {
      git { url = "github.com/x/s" }
      vars = { n = inputs.nope }
    }
  }
}
`,
		}},
		"outside a package": {map[string]string{
			"Alphasfile": "service \"go\" \"s\" {\n  git { url = \"github.com/x/s\" }\n  vars = { n = inputs.sites }\n}\n",
		}},
	}
	want := map[string]string{
		"unknown input":     `inputs.nope: package p has no input "nope" (inputs: sites)`,
		"outside a package": "inputs.sites: only a package has inputs",
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), c.files)
			_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
			if err == nil || !strings.Contains(err.Error(), want[hint]) {
				t.Fatalf("want %q, got %v", want[hint], err)
			}
		})
	}
}

func TestLoadTree_provideIsGone(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./proxy\" {\n  provide \"sites\" \"a\" {\n    host = \"a\"\n  }\n}\n",
		"proxy/Alphasfile": pkgProxy,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `Blocks of type "provide" are not expected here`) {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_mapInputOfNullsTakesItsDefault(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./proxy\" {\n  inputs = { sites = null }\n}\n",
		"proxy/Alphasfile": strings.Replace(pkgProxy, "default     = {}", `default     = { base = { host = "base.test", port = 1 } }`, 1),
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "base:base.test=1/false;" {
		t.Errorf("routes = %q; a map every import sets to null takes its default", got)
	}
}
