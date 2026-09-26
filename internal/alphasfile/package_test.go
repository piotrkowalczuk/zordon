package alphasfile

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const pkgWeb = `
service "go" "web" {
  git { url = "github.com/x/web" }
  vars = { port = 8080 }
}
`

func TestOpen_packageDirectoryImport(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" {}`,
		"web/Alphasfile": pkgWeb,
	})
	af := openTree(t, root)
	web := svcByName(af, "web/web")
	if web == nil || web.Module != "web" {
		t.Fatalf("services = %v", serviceNames(af))
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
	if got := serviceNames(openTree(t, root)); !equalStrs(got, []string{"edge/web"}) {
		t.Errorf("services = %v", got)
	}
}

func TestLoadTree_packageNameClashNeedsAlias(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":       "import \"./a/web\" {}\nimport \"./b/web\" {}\n",
		"a/web/Alphasfile": pkgWeb,
		"b/web/Alphasfile": pkgWeb,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), `another package is already named "web"`) || !strings.Contains(err.Error(), `import "./b/web" "<alias>" {}`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_packageModuleNameClash(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "module \"web\" {}\nimport \"./web\" {}\n",
		"web/Alphasfile": pkgWeb,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), `package "web" has the same name as module "web"`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_packageImportsMustAgree(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "import \"./db\" { features = [\"replica\"] }\nimport \"./app\" {}\n",
		"app/Alphasfile": `import "../db" {}`,
		"db/Alphasfile":  "feature \"replica\" {}\n",
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
require "../db" {}

service "go" "app" {
  git { url = "github.com/x/app" }
  vars = { db = module.db.service.go.db.vars.port }
}
`,
		"db/Alphasfile": `
input "port" { default = 5432 }

service "go" "db" {
  git { url = "github.com/x/db" }
  vars = { port = input.port }
}
`,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "app/app").Runtime.Vars["db"]); got != "5432" {
		t.Errorf("app db = %s, services = %v", got, serviceNames(af))
	}
}

func TestLoadTree_packageContentRules(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"env":       {`env = { A = "1" }`, "top-level env is decided by the entrypoint"},
		"dotenv":    {`dotenv = ".env"`, "top-level dotenv is decided by the entrypoint"},
		"workspace": {"workspace {\n  branch = \"x\"\n}\n", "top-level workspace is decided by the entrypoint"},
		"module":    {`module "m" {}`, `module "m" in a package`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":     `import "./p" {}`,
				"p/Alphasfile":   c.body + "\n",
				"p/Alphasfile.x": "",
			})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
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

func TestOpen_packageSysenvUnion(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     "sysenv = [\"HOME\"]\nimport \"./web\" {}\n",
		"web/Alphasfile": "sysenv = [\"TZ\"]\n" + pkgWeb,
	})
	if got := openTree(t, root).SysEnv; !equalStrs(got, []string{"HOME", "TZ"}) {
		t.Errorf("SysEnv = %v", got)
	}
}

func TestOpen_packageToolchainKey(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" {}`,
		"web/Alphasfile": "toolchain {\n  go { version = \"1.22.0\" }\n}\n" + pkgWeb,
	})
	af := openTree(t, root)
	if tc := af.Toolchain["web/go"]; tc == nil || tc.Version != "1.22.0" {
		t.Fatalf("toolchain = %v", toolchainKeys(af))
	}
	if got := svcByName(af, "web/web").ToolchainKey; got != "web/go" {
		t.Errorf("ToolchainKey = %q", got)
	}
}

func TestCompile_packageRunsOnItsOwn(t *testing.T) {
	src := `
input "greeting" { default = "hello" }
feature "extra" {}

service "go" "web" {
  git { url = "github.com/x/web" }
  vars = { greeting = input.greeting, extra = feature.extra }
}

service "go" "extra" {
  enabled = feature.extra
  git { url = "github.com/x/extra" }
}
`
	af, err := Compile("Alphasfile", []byte(src), testInv(), nil, testCfgHash, TestConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got := serviceNames(af); !equalStrs(got, []string{"web"}) {
		t.Fatalf("services = %v, want only web: features are off when a package runs on its own", got)
	}
	web := svcByName(af, "web")
	if got := fmt.Sprint(web.Runtime.Vars["greeting"], " ", web.Runtime.Vars["extra"]); got != "hello false" {
		t.Errorf("vars = %s", got)
	}
}

func TestOpen_packageVisibilityHintNamesDirectory(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": "import \"./app\" {}\nimport \"./db\" {}\n",
		"app/Alphasfile": `
service "go" "app" {
  git { url = "github.com/x/app" }
  vars = { db = module.db.service.go.db.name }
}
`,
		"db/Alphasfile": "service \"go\" \"db\" {\n  git { url = \"github.com/x/db\" }\n}\n",
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `add require "../db" {} inside package "app"`) {
		t.Fatalf("got %v", err)
	}
}
