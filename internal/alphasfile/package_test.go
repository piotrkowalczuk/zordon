package alphasfile

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const pkgWeb = `
package "web" {
  component "web" {
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
	if web.ID() != "package.web.component.web.service.go.web" {
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
  component "db" {
    service "go" "db" {
      git { url = "github.com/x/db" }
      vars = { port = 5432 }
    }
  }
  component "api" {
    service "go" "api" {
      git { url = "github.com/x/api" }
      vars = { db = component.db.service.go.db.vars.port }
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
		"Alphasfile":     "component \"web\" {}\nimport \"./web\" {}\n",
		"web/Alphasfile": pkgWeb,
	})
	if got := serviceNames(openTree(t, root)); !equalStrs(got, []string{"web/web/web"}) {
		t.Errorf("services = %v; package.web and component.web are separate names", got)
	}
}

const pkgDBReplica = `
package "db" {
  features = { replica = { description = "Runs a read replica next to the primary" } }

  component "db" {
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
		"invalid package name":   {"package \"no spaces\" {}\n", `invalid package name "no spaces"`},
		"invalid component name": {"package \"p\" {\n  component \"no spaces\" {}\n}\n", `invalid component name "no spaces"`},
		"duplicate module":       {"package \"p\" {\n  component \"m\" {}\n  component \"m\" {}\n}\n", `duplicate component "m" in package "p"`},
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
  component "db" {
    service "go" "replica" {
      enabled = features.replica
      git { url = "github.com/x/db" }
    }
  }
}
`

func TestLoadTree_referenceToSwitchedOffServiceOfASiblingModule(t *testing.T) {
	store := strings.Replace(pkgStore, "\n}\n", `
  component "api" {
    service "go" "api" {
      git { url = "github.com/x/api" }
      vars = { db = component.db.service.go.replica.name }
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
		"app/Alphasfile":   "package \"app\" {\n  import \"../store\" {}\n  component \"a\" {\n    service \"go\" \"a\" {\n      git { url = \"github.com/x/a\" }\n      vars = { db = package.store.component.db.service.go.replica.name }\n    }\n  }\n}\n",
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

  component "app" {
    service "go" "app" {
      git { url = "github.com/x/app" }
      vars = { db = package.db.component.db.service.go.db.vars.port }
    }
  }
}
`,
		"db/Alphasfile": `
package "db" {
  inputs = { port = { description = "Port to listen on.", type = number, default = 5432 } }

  component "db" {
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
		"module":    {`component "m" {}`, `component "m" outside package "p"`},
		"service":   {"service \"go\" \"s\" {\n  git { url = \"github.com/x/s\" }\n}", `service outside package "p"`},
		"second":    {`package "q" {}`, "a file holds one package block"},
		"import":    {`import "./x" {}`, `import "./x" outside package "p"`},
		"require":   {`require "github.com/a/b" { ref = "main" }`, `require "github.com/a/b" outside package "p"`},
		"toolchain": {"toolchain {\n  go { version = \"1.22.0\" }\n}", `toolchain outside package "p"`},
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
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `has no package block, so it is not a package; wrap its components in package "<name>" {}`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_packageDirWithoutAlphasfile(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       `import "./web" {}`,
		"web/Alphasfile.x": `component "m" {}`,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "has no Alphasfile, so it is not a package") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_packageImportRejectsModules(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" { components = ["web"] }`,
		"web/Alphasfile": pkgWeb,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "a package is imported whole; drop components") {
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
  component "web" {
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

  component "web" {
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
  component "app" {
    service "go" "app" {
      git { url = "github.com/x/app" }
      vars = { db = package.db.component.db.service.go.db.name }
    }
  }
}
`,
		"db/Alphasfile": "package \"db\" {\n  component \"db\" {\n    service \"go\" \"db\" {\n      git { url = \"github.com/x/db\" }\n    }\n  }\n}\n",
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `package.db is not visible in package "app"`) || !strings.Contains(err.Error(), `add import "../db" {} inside package "app"`) {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_moduleRefInsidePackageIsASibling(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": "component \"db\" {}\nimport \"./app\" {}\n",
		"app/Alphasfile": `
package "app" {
  component "api" {
    service "go" "api" {
      git { url = "github.com/x/api" }
      vars = { db = component.db.service.go.db.name }
    }
  }
}
`,
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `package "app" has no component "db"`) {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_moduleRequiresAPackage(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `
component "gw" {
  import "./web" {}

  service "go" "gw" {
    git { url = "github.com/x/gw" }
    vars = { upstream = package.web.component.web.service.go.web.vars.port }
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

func TestLoadTree_packageKeepsOneName(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "import \"./db\" {}\nimport \"./app\" {}\n",
		"db/Alphasfile":  `package "db" {}`,
		"app/Alphasfile": "package \"app\" {\n  import \"../db\" \"pg\" {}\n}\n",
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `this package is already in the stack as "db"`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_packageCannotImportTheEntrypoint(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   `import "./p" {}`,
		"p/Alphasfile": "package \"p\" {\n  import \"../\" {}\n}\n",
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "is the entrypoint, not a package") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_packageNameClashesWithTheEntrypoint(t *testing.T) {
	cases := map[string]struct{ entry, want string }{
		"top-level service": {"service \"go\" \"gw\" {\n  git { url = \"github.com/x/gw\" }\n}\nimport \"./gw\" {}\n", `package "gw" has the same name as the top-level service declared at`},
		"module":            {"component \"gw\" {\n  service \"go\" \"web\" {\n    git { url = \"github.com/x/web\" }\n  }\n}\nimport \"./gw\" {}\n", `package "gw" has component "web", and component "gw" declared at`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":    c.entry,
				"gw/Alphasfile": "package \"gw\" {\n  component \"web\" {\n    service \"go\" \"x\" {\n      git { url = \"github.com/x/x\" }\n    }\n  }\n}\n",
			})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "import it with an alias") {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_invalidPackageAlias(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":    `import "./db" "no spaces" {}`,
		"db/Alphasfile": `package "db" {}`,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `"no spaces" is not a valid package name`) {
		t.Fatalf("got %v", err)
	}
}
