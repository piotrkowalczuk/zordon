package alphasfile

import (
	"fmt"
	"strings"
	"testing"
)

const pkgProxy = `
package "proxy" {
  slots = {
    sites = {
      description = "Hosts to route."
      entry       = object({ host = string, port = number, tls = optional(bool, false) })
      unique      = ["host"]
    }
  }

  module "proxy" {
    service "go" "proxy" {
      git { url = "github.com/x/proxy" }
      vars = { routes = "%{ for k, s in slots.sites }${s.key}:${s.host}=${s.port}/${s.tls};%{ endfor }" }
    }
  }
}
`

// pkgSite is a site that registers itself with the proxy it imports.
func pkgSite(name string, port int) string {
	return fmt.Sprintf(`
package %[1]q {
  import "../proxy" {
    provide "sites" {
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

func TestOpen_provideFillsASlot(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./shop\" {}\nimport \"./blog\" {}\n",
		"proxy/Alphasfile": pkgProxy,
		"shop/Alphasfile":  pkgSite("shop", 8081),
		"blog/Alphasfile":  pkgSite("blog", 8082),
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "blog:blog.test=8082/false;shop:shop.test=8081/false;" {
		t.Errorf("routes = %q; each site provides one entry keyed by its package, evaluated before the proxy reads it, with defaults applied", got)
	}
}

func TestOpen_provideKeysAreNamespacedByProvider(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./a\" {}\nimport \"./b\" {}\n",
		"proxy/Alphasfile": pkgProxy,
		"a/Alphasfile":     "package \"a\" {\n  import \"../proxy\" {\n    provide \"sites\" \"api\" {\n      host = \"a.test\"\n      port = 1\n    }\n  }\n}\n",
		"b/Alphasfile":     "package \"b\" {\n  import \"../proxy\" {\n    provide \"sites\" \"api\" {\n      host = \"b.test\"\n      port = 2\n      tls  = true\n    }\n  }\n}\n",
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "a.api:a.test=1/false;b.api:b.test=2/true;" {
		t.Errorf("routes = %q; the same key from two packages does not collide", got)
	}
}

func TestOpen_entrypointProvidesWithAKey(t *testing.T) {
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
	if got := fmt.Sprint(svcByName(af, "proxy/proxy/proxy").Runtime.Vars["routes"]); got != "docs:docs.test=9000/false;" {
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
    provide "sites" {
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
			`provide "nope": package proxy has no slot "nope" (slots: sites)`,
		},
		"repeated key": {
			"import \"./proxy\" {\n  provide \"sites\" \"a\" {\n    host = \"a\"\n    port = 1\n  }\n  provide \"sites\" \"a\" {\n    host = \"b\"\n    port = 2\n  }\n}\n",
			`entry "a" of slot "sites" in package proxy is already provided at`,
		},
		"no key at the entrypoint": {
			"import \"./proxy\" {\n  provide \"sites\" {\n    host = \"a\"\n    port = 1\n  }\n}\n",
			`provide "sites" at the entrypoint's top level needs a key: provide "sites" "<key>" {}`,
		},
		"nested block": {
			"import \"./proxy\" {\n  provide \"sites\" \"a\" {\n    tls {}\n  }\n}\n",
			`provide "sites" takes attributes only`,
		},
		"bad key": {
			"import \"./proxy\" {\n  provide \"sites\" \"no spaces\" {\n    host = \"a\"\n  }\n}\n",
			"use letters, digits, '_' or '-' for the key",
		},
		"unknown attribute": {
			"import \"./proxy\" {\n  provide \"sites\" \"a\" {\n    hots = \"a\"\n    port = 1\n  }\n}\n",
			`slot "sites" of package proxy has no attribute "hots" (entry: host, port, tls (optional))`,
		},
		"missing attribute": {
			"import \"./proxy\" {\n  provide \"sites\" \"a\" {\n    host = \"a\"\n  }\n}\n",
			`slot "sites" of package proxy needs attribute "port"`,
		},
		"fragment": {
			"import \"./Alphasfile.f\" {\n  modules = [\"m\"]\n  provide \"sites\" \"a\" {\n    host = \"a\"\n  }\n}\n",
			"provide fills a slot of a package, not a fragment file",
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

func TestOpen_provideValueMustFitTheEntry(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./proxy\" {\n  provide \"sites\" \"a\" {\n    host = \"a\"\n    port = \"not a number\"\n  }\n}\n",
		"proxy/Alphasfile": pkgProxy,
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `provide "sites": the entry does not fit slot "sites" of package proxy`) {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_provideDetectsAUniqueCollision(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./shop\" {}\nimport \"./proxy\" {\n  provide \"sites\" \"legacy\" {\n    host = \"shop.test\"\n    port = 1\n  }\n}\n",
		"proxy/Alphasfile": pkgProxy,
		"shop/Alphasfile":  pkgSite("shop", 8081),
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `host = "shop.test" is already provided by entry`) || !strings.Contains(err.Error(), `slot "sites" of package proxy takes each host once`) {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_slotErrors(t *testing.T) {
	cases := map[string]struct{ files map[string]string }{
		"unknown slot": {map[string]string{
			"Alphasfile": `import "./p" {}`,
			"p/Alphasfile": `
package "p" {
  slots = { sites = { description = "Sites", entry = object({ host = string }) } }
  module "m" {
    service "go" "s" {
      git { url = "github.com/x/s" }
      vars = { n = slots.nope }
    }
  }
}
`,
		}},
		"outside a package": {map[string]string{
			"Alphasfile": "service \"go\" \"s\" {\n  git { url = \"github.com/x/s\" }\n  vars = { n = slots.sites }\n}\n",
		}},
	}
	want := map[string]string{
		"unknown slot":      `slots.nope: package p has no slot "nope" (slots: sites)`,
		"outside a package": "slots.sites: only a package reads its slots",
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

func TestLoadTree_slotDeclarationErrors(t *testing.T) {
	cases := map[string]struct{ slots, want string }{
		"not an object":       {`["sites"]`, `slots of package "p" map each slot to { description = "...", entry = object({ ... }) }`},
		"no description":      {`{ sites = { entry = object({ host = string }) } }`, `slot "sites" of package "p" needs a description`},
		"no entry":            {`{ sites = { description = "Sites" } }`, `slot "sites" of package "p" needs entry = object({ ... })`},
		"not an object entry": {`{ sites = { description = "Sites", entry = string } }`, `slot "sites": entry must be object({ ... })`},
		"reserved key":        {`{ sites = { description = "Sites", entry = object({ key = string }) } }`, `slot "sites": entry cannot declare key`},
		"bad type":            {`{ sites = { description = "Sites", entry = object({ host = strin }) } }`, `slot "sites": entry:`},
		"unknown field":       {`{ sites = { description = "Sites", entry = object({ host = string }), max = 3 } }`, `slot "sites" takes description, entry and unique only`},
		"unique unknown":      {`{ sites = { description = "Sites", entry = object({ host = string }), unique = ["port"] } }`, `slot "sites": unique names "port", which entry does not declare`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":   `import "./p" {}`,
				"p/Alphasfile": "package \"p\" {\n  slots = " + c.slots + "\n}\n",
			})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
