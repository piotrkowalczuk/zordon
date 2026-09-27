package alphasfile

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const pkgWeb = `
package "web" {
  module "web" {
    service "go" "web" {
      git { url = "github.com/x/web" }
      vars = { port = 8080 }
    }
  }
}
`

func TestOpen_packageDirectoryImport(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" {}`,
		"web/Alphasfile": pkgWeb,
	})
	af := openTree(t, root)
	web := svcByName(af, "web/web/web")
	if web == nil || web.Module != "web/web" {
		t.Fatalf("services = %v", serviceNames(af))
	}
	if web.ID() != "package.web.module.web.service.go.web" {
		t.Errorf("id = %q", web.ID())
	}
	if got := fmt.Sprint(web.Runtime.Vars["port"]); got != "8080" {
		t.Errorf("port = %s", got)
	}
}

func TestOpen_packageAlias(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" "edge" {}`,
		"web/Alphasfile": pkgWeb,
	})
	if got := serviceNames(openTree(t, root)); !equalStrs(got, []string{"edge/web/web"}) {
		t.Errorf("services = %v", got)
	}
}

func TestOpen_packageModulesSeeEachOther(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./app" {}`,
		"app/Alphasfile": `
package "app" {
  module "db" {
    service "go" "db" {
      git { url = "github.com/x/db" }
      vars = { port = 5432 }
    }
  }
  module "api" {
    service "go" "api" {
      git { url = "github.com/x/api" }
      vars = { db = module.db.service.go.db.vars.port }
    }
  }
}
`,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "app/api/api").Runtime.Vars["db"]); got != "5432" {
		t.Errorf("api db = %s, services = %v", got, serviceNames(af))
	}
}

func TestLoadTree_packageNameClashNeedsAlias(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./a/web\" {}\nimport \"./b/web\" {}\n",
		"a/web/Alphasfile": pkgWeb,
		"b/web/Alphasfile": pkgWeb,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), `another package is already named "web"`) || !strings.Contains(err.Error(), "alias") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_packageAndModuleMayShareAName(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "module \"web\" {}\nimport \"./web\" {}\n",
		"web/Alphasfile": pkgWeb,
	})
	if got := serviceNames(openTree(t, root)); !equalStrs(got, []string{"web/web/web"}) {
		t.Errorf("services = %v; package.web and module.web are separate names", got)
	}
}

func TestLoadTree_packageImportsMustAgree(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "import \"./db\" { features = [\"replica\"] }\nimport \"./app\" {}\n",
		"app/Alphasfile": "package \"app\" {\n  import \"../db\" {}\n}\n",
		"db/Alphasfile":  "package \"db\" {\n  features = [\"replica\"]\n}\n",
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), "passes other inputs or features than the import at") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_packageRequiredUsesDefaults(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./app" {}`,
		"app/Alphasfile": `
package "app" {
  require "../db" {}

  module "app" {
    service "go" "app" {
      git { url = "github.com/x/app" }
      vars = { db = package.db.module.db.service.go.db.vars.port }
    }
  }
}
`,
		"db/Alphasfile": `
package "db" {
  inputs = { port = 5432 }

  module "db" {
    service "go" "db" {
      git { url = "github.com/x/db" }
      vars = { port = inputs.port }
    }
  }
}
`,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "app/app/app").Runtime.Vars["db"]); got != "5432" {
		t.Errorf("app db = %s, services = %v", got, serviceNames(af))
	}
}

func TestLoadTree_packageFileHoldsOnlyItsBlock(t *testing.T) {
	cases := map[string]struct{ extra, want string }{
		"env":       {`env = { A = "1" }`, `env outside package "p"`},
		"dotenv":    {`dotenv = ".env"`, `dotenv outside package "p"`},
		"sysenv":    {`sysenv = ["HOME"]`, "host variables are passed by the entrypoint"},
		"workspace": {"workspace {\n  branch = \"x\"\n}", `workspace outside package "p"`},
		"module":    {`module "m" {}`, `module "m" outside package "p"`},
		"service":   {"service \"go\" \"s\" {\n  git { url = \"github.com/x/s\" }\n}", `service outside package "p"`},
		"second":    {`package "q" {}`, "a file holds one package block"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":   `import "./p" {}`,
				"p/Alphasfile": "package \"p\" {}\n" + c.extra + "\n",
			})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_packageBlockRejectsEntrypointSections(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   `import "./p" {}`,
		"p/Alphasfile": "package \"p\" {\n  sysenv = [\"HOME\"]\n}\n",
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `An argument named "sysenv" is not expected here`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTreeWith_packageThatIsFederationLevel(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":     `import "./web" {}`,
		"web/Alphasfile": pkgWeb,
	})
	_, err := LoadTreeWith(root, LoadOptions{Chain: []string{filepath.Join(dir, "web", "Alphasfile"), root}})
	if err == nil || !strings.Contains(err.Error(), "is a federation level of this invocation") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_directoryWithoutPackageBlock(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" {}`,
		"web/Alphasfile": "service \"go\" \"web\" {\n  git { url = \"github.com/x/web\" }\n}\n",
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `has no package block, so it is not a package; wrap its modules in package "<name>" {}`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_packageDirWithoutAlphasfile(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       `import "./web" {}`,
		"web/Alphasfile.x": `module "m" {}`,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "has no Alphasfile, so it is not a package") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_packageImportRejectsModules(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" { modules = ["web"] }`,
		"web/Alphasfile": pkgWeb,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "a package is imported whole; drop modules") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_packageCannotRequireFragments(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./app" {}`,
		"app/Alphasfile": "package \"app\" {\n  require \"../Alphasfile.f\" { modules = [\"m\"] }\n}\n",
		"Alphasfile.f":   `module "m" {}`,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "a package depends on other packages, not on a fragment's modules") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_packageToolchainPinsItsModules(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" {}`,
		"web/Alphasfile": `
package "web" {
  toolchain {
    go { version = "1.22.0" }
  }
  module "web" {
    service "go" "web" {
      git { url = "github.com/x/web" }
    }
  }
}
`,
	})
	af := openTree(t, root)
	if tc := af.Toolchain["web/web/go"]; tc == nil || tc.Version != "1.22.0" {
		t.Fatalf("toolchain = %v", toolchainKeys(af))
	}
	if got := svcByName(af, "web/web/web").ToolchainKey; got != "web/web/go" {
		t.Errorf("ToolchainKey = %q", got)
	}
}

func TestLoadTree_packageRunsOnItsOwn(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `
package "web" {
  inputs   = { greeting = "hello" }
  features = ["extra"]

  module "web" {
    service "go" "web" {
      git { url = "github.com/x/web" }
      vars = { greeting = inputs.greeting, extra = features.extra }
    }
    service "go" "extra" {
      enabled = features.extra
      git { url = "github.com/x/extra" }
    }
  }
}
`,
	})
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"web/web/web"}) {
		t.Fatalf("services = %v, want only web: features are off when a package runs on its own", got)
	}
	web := svcByName(af, "web/web/web")
	if got := fmt.Sprint(web.Runtime.Vars["greeting"], " ", web.Runtime.Vars["extra"]); got != "hello false" {
		t.Errorf("vars = %s", got)
	}
}

func TestOpen_packageVisibilityHintNamesDirectory(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": "import \"./app\" {}\nimport \"./db\" {}\n",
		"app/Alphasfile": `
package "app" {
  module "app" {
    service "go" "app" {
      git { url = "github.com/x/app" }
      vars = { db = package.db.module.db.service.go.db.name }
    }
  }
}
`,
		"db/Alphasfile": "package \"db\" {\n  module \"db\" {\n    service \"go\" \"db\" {\n      git { url = \"github.com/x/db\" }\n    }\n  }\n}\n",
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `package.db is not visible in package "app"`) || !strings.Contains(err.Error(), `add require "../db" {} inside package "app"`) {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_moduleRefInsidePackageIsASibling(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": "module \"db\" {}\nimport \"./app\" {}\n",
		"app/Alphasfile": `
package "app" {
  module "api" {
    service "go" "api" {
      git { url = "github.com/x/api" }
      vars = { db = module.db.service.go.db.name }
    }
  }
}
`,
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `package "app" has no module "db"`) {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_moduleRequiresAPackage(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `
module "gw" {
  require "./web" {}

  service "go" "gw" {
    git { url = "github.com/x/gw" }
    vars = { upstream = package.web.module.web.service.go.web.vars.port }
  }
}
`,
		"web/Alphasfile": pkgWeb,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "gw/gw").Runtime.Vars["upstream"]); got != "8080" {
		t.Errorf("upstream = %s, services = %v", got, serviceNames(af))
	}
}
