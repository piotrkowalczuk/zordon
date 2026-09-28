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

const pkgDBReplica = `
package "db" {
  features = { replica = { description = "Runs a read replica next to the primary" } }

  module "db" {
    service "go" "primary" {
      git { url = "github.com/x/db" }
    }
    service "go" "replica" {
      enabled = features.replica
      git { url = "github.com/x/db" }
    }
  }
}
`

func TestLoadTree_packageAndModuleNames(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"invalid package name": {"package \"no spaces\" {}\n", `invalid package name "no spaces"`},
		"invalid module name":  {"package \"p\" {\n  module \"no spaces\" {}\n}\n", `invalid module name "no spaces"`},
		"duplicate module":     {"package \"p\" {\n  module \"m\" {}\n  module \"m\" {}\n}\n", `duplicate module "m" in package "p"`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": `import "./p" {}`, "p/Alphasfile": c.body})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_entrypointImportsMustAgreeOnInputs(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         "import \"./greeter\" { inputs = { name = \"a\" } }\nimport \"./greeter\" { inputs = { name = \"b\" } }\n",
		"greeter/Alphasfile": pkgGreeter,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "passes other inputs or features than the import at") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_importListsAFeatureTwice(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":    `import "./db" { features = ["replica", "replica"] }`,
		"db/Alphasfile": pkgDBReplica,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `import "./db" lists feature "replica" twice`) {
		t.Fatalf("got %v", err)
	}
}

const pkgStore = `
package "store" {
  features = { replica = { description = "Runs a replica." } }
  module "db" {
    service "go" "replica" {
      enabled = features.replica
      git { url = "github.com/x/db" }
    }
  }
}
`

func TestLoadTree_referenceToSwitchedOffServiceOfASiblingModule(t *testing.T) {
	store := strings.Replace(pkgStore, "\n}\n", `
  module "api" {
    service "go" "api" {
      git { url = "github.com/x/api" }
      vars = { db = module.db.service.go.replica.name }
    }
  }
}
`, 1)
	root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": `import "./store" {}`, "store/Alphasfile": store})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `references service "go" "replica", which is switched off by enabled at`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_referenceToSwitchedOffServiceOfAnotherPackage(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./app\" {}\n",
		"store/Alphasfile": pkgStore,
		"app/Alphasfile":   "package \"app\" {\n  import \"../store\" {}\n  module \"a\" {\n    service \"go\" \"a\" {\n      git { url = \"github.com/x/a\" }\n      vars = { db = package.store.module.db.service.go.replica.name }\n    }\n  }\n}\n",
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "app/Alphasfile") || !strings.Contains(err.Error(), `references service "go" "replica", which is switched off by enabled at`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_entrypointImportsMustAgree(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":    "import \"./db\" { features = [\"replica\"] }\nimport \"./db\" {}\n",
		"db/Alphasfile": pkgDBReplica,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), "passes other inputs or features than the import at") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_packageImportTurnsOnAFeature(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./app" {}`,
		"app/Alphasfile": "package \"app\" {\n  import \"../db\" { features = [\"replica\"] }\n}\n",
		"db/Alphasfile":  pkgDBReplica,
	})
	if got := serviceNames(openTree(t, root)); !equalStrs(got, []string{"db/db/primary", "db/db/replica"}) {
		t.Errorf("services = %v", got)
	}
}

func TestOpen_packageImportsUniteFeatures(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "import \"./app\" {}\nimport \"./ops\" {}\n",
		"app/Alphasfile": "package \"app\" {\n  import \"../db\" { features = [\"replica\"] }\n}\n",
		"ops/Alphasfile": "package \"ops\" {\n  import \"../db\" {}\n}\n",
		"db/Alphasfile":  pkgDBReplica,
	})
	if got := serviceNames(openTree(t, root)); !equalStrs(got, []string{"db/db/primary", "db/db/replica"}) {
		t.Errorf("services = %v", got)
	}
}

func TestOpen_entrypointImportIsASuperset(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "import \"./db\" { features = [\"replica\"] }\nimport \"./app\" {}\n",
		"app/Alphasfile": "package \"app\" {\n  import \"../db\" {}\n}\n",
		"db/Alphasfile":  pkgDBReplica,
	})
	if got := serviceNames(openTree(t, root)); !equalStrs(got, []string{"db/db/primary", "db/db/replica"}) {
		t.Errorf("services = %v", got)
	}
}

func TestLoadTree_entrypointLeavesANeededFeatureOff(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "import \"./db\" {}\nimport \"./app\" {}\n",
		"app/Alphasfile": "package \"app\" {\n  import \"../db\" { features = [\"replica\"] }\n}\n",
		"db/Alphasfile":  pkgDBReplica,
	})
	_, err := LoadTree(root)
	want := []string{
		"Alphasfile:1",
		`package db runs with feature "replica" off, but package app needs it`,
		"  replica: Runs a read replica next to the primary",
		`turn it on here with features = ["replica"], or drop that import`,
	}
	for _, w := range want {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Fatalf("missing %q in %v", w, err)
		}
	}
}

func TestLoadTree_entrypointLeavesANeededFeatureOffBehindAFeature(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "import \"./db\" {}\nimport \"./app\" { features = [\"ha\"] }\n",
		"app/Alphasfile": "package \"app\" {\n  features = { ha = { description = \"Survives a database restart\" } }\n  import \"../db\" {\n    enabled  = features.ha\n    features = [\"replica\"]\n  }\n}\n",
		"db/Alphasfile":  pkgDBReplica,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), "or turn off what enables that import in package app (features ha)") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_packagesImportingEachOther(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   `import "./a" {}`,
		"a/Alphasfile": "package \"a\" {\n  import \"../b\" {}\n}\n",
		"b/Alphasfile": "package \"b\" {\n  import \"../c\" {}\n}\n",
		"c/Alphasfile": "package \"c\" {\n  import \"../b\" {}\n}\n",
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), "packages b, c import each other, so none of them can be configured first; import one of them at the top of the entrypoint") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_entrypointBreaksAnImportCycle(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   "import \"./a\" {}\nimport \"./b\" {}\n",
		"a/Alphasfile": "package \"a\" {\n  import \"../b\" {}\n}\n",
		"b/Alphasfile": "package \"b\" {\n  import \"../a\" {}\n}\n",
	})
	openTree(t, root)
}

func TestOpen_packageRequiredUsesDefaults(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./app" {}`,
		"app/Alphasfile": `
package "app" {
  import "../db" {}

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
  inputs = { port = { description = "Port to listen on.", type = number, default = 5432 } }

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
		"app/Alphasfile": "package \"app\" {\n  import \"../Alphasfile.f\" { modules = [\"m\"] }\n}\n",
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
  inputs   = { greeting = { description = "How to greet.", type = string, default = "hello" } }
  features = { extra = { description = "Adds the extra blocks" } }

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
	if err == nil || !strings.Contains(err.Error(), `package.db is not visible in package "app"`) || !strings.Contains(err.Error(), `add import "../db" {} inside package "app"`) {
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
  import "./web" {}

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
