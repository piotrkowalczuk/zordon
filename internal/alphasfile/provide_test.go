package alphasfile

import (
	"fmt"
	"strings"
	"testing"
)

const pkgProxy = `
package "proxy" {
  collect = { sites = "Virtual hosts: { host, port }" }

  module "proxy" {
    service "go" "proxy" {
      git { url = "github.com/x/proxy" }
      vars = { routes = "%{ for k, s in collected.sites }${k}:${s.host}=${s.port};%{ endfor }" }
    }
  }
}
`

// pkgSite is a site that registers itself with the proxy it imports.
func pkgSite(name string, port int) string {
	return fmt.Sprintf(`
package %[1]q {
  import "../proxy" {
    provide "sites" %[1]q {
      host = "%[1]s.test"
      port = module.site.service.go.site.vars.port
    }
  }

  module "site" {
    service "go" "site" {
      git { url = "github.com/x/site" }
      vars = { port = %[2]d }
    }
  }
}
`, name, port)
}

func TestOpen_provideFillsACollectedSlot(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./shop\" {}\nimport \"./blog\" {}\n",
		"proxy/Alphasfile": pkgProxy,
		"shop/Alphasfile":  pkgSite("shop", 8081),
		"blog/Alphasfile":  pkgSite("blog", 8082),
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "blog:blog.test=8082;shop:shop.test=8081;" {
		t.Errorf("routes = %q; each site provides its entry, evaluated before the proxy reads it", got)
	}
}

func TestOpen_entrypointProvides(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `
import "./proxy" {
  provide "sites" "docs" {
    host = "docs.test"
    port = 9000
  }
}
`,
		"proxy/Alphasfile": pkgProxy,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "docs:docs.test=9000;" {
		t.Errorf("routes = %q", got)
	}
}

func TestOpen_emptySlot(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       `import "./proxy" {}`,
		"proxy/Alphasfile": pkgProxy,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "" {
		t.Errorf("routes = %q; a slot nobody fills is empty", got)
	}
}

func TestOpen_switchedOffImportProvidesNothing(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./proxy\" {}\nimport \"./shop\" {}\n",
		"proxy/Alphasfile": pkgProxy,
		"shop/Alphasfile": `
package "shop" {
  features = { public = "Serves the shop through the proxy." }

  import "../proxy" {
    enabled = features.public
    provide "sites" "shop" {
      host = "shop.test"
      port = 1
    }
  }
}
`,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "" {
		t.Errorf("routes = %q", got)
	}
}

func TestLoadTree_provideErrors(t *testing.T) {
	cases := map[string]struct{ entry, want string }{
		"unknown slot": {
			"import \"./proxy\" {\n  provide \"nope\" \"a\" {\n    host = \"a\"\n  }\n}\n",
			`provide "nope" "a": package proxy does not collect "nope" (collects: sites)`,
		},
		"repeated key": {
			"import \"./proxy\" {\n  provide \"sites\" \"a\" {\n    host = \"a\"\n  }\n  provide \"sites\" \"a\" {\n    host = \"b\"\n  }\n}\n",
			`provide "sites" "a" to package proxy is already provided at`,
		},
		"nested block": {
			"import \"./proxy\" {\n  provide \"sites\" \"a\" {\n    tls {}\n  }\n}\n",
			`provide "sites" "a" takes attributes only`,
		},
		"bad key": {
			"import \"./proxy\" {\n  provide \"sites\" \"no spaces\" {\n    host = \"a\"\n  }\n}\n",
			"use letters, digits, '_' or '-' for the key",
		},
		"fragment": {
			"import \"./Alphasfile.f\" {\n  modules = [\"m\"]\n  provide \"sites\" \"a\" {\n    host = \"a\"\n  }\n}\n",
			"provide fills a slot a package collects, not a fragment file",
		},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":       c.entry,
				"Alphasfile.f":     `module "m" {}`,
				"proxy/Alphasfile": pkgProxy,
			})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_provideKeyHasOneSource(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./shop\" {}\nimport \"./proxy\" {\n  provide \"sites\" \"shop\" {\n    host = \"x\"\n  }\n}\n",
		"proxy/Alphasfile": pkgProxy,
		"shop/Alphasfile":  pkgSite("shop", 1),
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "each key has one source") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_collectedErrors(t *testing.T) {
	cases := map[string]struct{ files map[string]string }{
		"unknown slot": {map[string]string{
			"Alphasfile": `import "./p" {}`,
			"p/Alphasfile": `
package "p" {
  collect = { sites = "Sites" }
  module "m" {
    service "go" "s" {
      git { url = "github.com/x/s" }
      vars = { n = length(collected.nope) }
    }
  }
}
`,
		}},
		"outside a package": {map[string]string{
			"Alphasfile": "service \"go\" \"s\" {\n  git { url = \"github.com/x/s\" }\n  vars = { n = collected.sites }\n}\n",
		}},
	}
	want := map[string]string{
		"unknown slot":      `collected.nope: package p does not collect "nope" (collects: sites)`,
		"outside a package": "collected.sites: only a package reads what it collects",
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

func TestLoadTree_collectIsDescribed(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   `import "./p" {}`,
		"p/Alphasfile": "package \"p\" {\n  collect = { sites = \"\" }\n}\n",
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `slot "sites" of package "p" needs a description of the entries it takes`) {
		t.Fatalf("got %v", err)
	}
}
