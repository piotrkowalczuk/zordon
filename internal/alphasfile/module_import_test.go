package alphasfile

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTree_unusedModuleImportStaysOut(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile": `import "shared/Alphasfile.shared" { modules = ["kafka"] }`,
		"shared/Alphasfile.shared": `
module "kafka" {
  service "go" "kafka" {
    git { url = "github.com/x/kafka" }
  }
}

module "kafka-ui" {
  import "../grafana/Alphasfile.grafana" { modules = ["grafana"] }

  service "go" "ui" {
    git { url = "github.com/x/ui" }
    vars = { grafana = module.grafana.service.go.grafana.name }
  }
}
`,
		"grafana/Alphasfile.grafana": `
module "grafana" {
  service "go" "grafana" {
    git { url = "github.com/x/grafana" }
  }
}
`,
	})
	tree, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := moduleNames(tree); !equalStrs(got, []string{"kafka"}) {
		t.Errorf("stack = %v, want only kafka: grafana is needed by kafka-ui alone", got)
	}
	var unused []string
	for _, u := range tree.Unused() {
		unused = append(unused, u.Module)
	}
	if !equalStrs(unused, []string{"kafka-ui", "grafana"}) {
		t.Errorf("unused = %v", unused)
	}
	for _, e := range tree.Imports() {
		if strings.HasSuffix(e.Path, "Alphasfile.grafana") {
			t.Errorf("an import inside a module outside the stack must not be listed: %+v", tree.Imports())
		}
	}
	if got := serviceNames(openTree(t, root)); !equalStrs(got, []string{"kafka/kafka"}) {
		t.Errorf("services = %v", got)
	}
}

func TestLoadTree_brokenImportInUnusedModuleFails(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "Alphasfile.f" { modules = ["used"] }`,
		"Alphasfile.f": `
module "used" {}

module "spare" {
  import "Alphasfile.missing" { modules = ["x"] }
}
`,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "Alphasfile.missing") {
		t.Fatalf("a broken import fails even in a module outside the stack, got %v", err)
	}
}

func TestLoadTree_rejectsTopLevelImportInFragment(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   `import "Alphasfile.a" { modules = ["a"] }`,
		"Alphasfile.a": "import \"Alphasfile.b\" { modules = [\"b\"] }\nmodule \"a\" {}\n",
		"Alphasfile.b": `module "b" {}`,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), "Alphasfile.a:1") || !strings.Contains(err.Error(), "move it into the module block that uses it") {
		t.Fatalf("got %v", err)
	}
}

func TestParseTree_rejectsModuleImport(t *testing.T) {
	_, err := ParseTree("test.hcl", []byte("module \"a\" {\n  import \"Alphasfile.f\" { modules = [\"m\"] }\n}\n"))
	if err == nil || !strings.Contains(err.Error(), "imports need a file on disk") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_moduleImportNotVisibleToSibling(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "apps/Alphasfile.apps" { modules = ["a", "c"] }`,
		"apps/Alphasfile.apps": `
module "a" {
  import "../b/Alphasfile.b" { modules = ["b"] }
}

module "c" {
  service "go" "c" {
    git { url = "github.com/x/c" }
    vars = { p = module.b.service.go.b.vars.port }
  }
}
`,
		"b/Alphasfile.b": `
module "b" {
  service "go" "b" {
    git { url = "github.com/x/b" }
    vars = { port = 7000 }
  }
}
`,
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil {
		t.Fatal("module a's import must not make module.b visible in module c")
	}
	for _, want := range []string{`module.b is not visible in module "c"`, `add import "../b/Alphasfile.b" { modules = ["b"] } inside module "c"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestOpen_fragmentSiblingNeedsImport(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "Alphasfile.f" { modules = ["a"] }`,
		"Alphasfile.f": `
module "a" {
  service "go" "a" {
    git { url = "github.com/x/a" }
    vars = { p = module.s.service.go.s.vars.port }
  }
}

module "s" {
  service "go" "s" {
    git { url = "github.com/x/s" }
    vars = { port = 7000 }
  }
}
`,
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `add import "Alphasfile.f" { modules = ["s"] } inside module "a"`) {
		t.Fatalf("a sibling in a fragment is imported like any module, got %v", err)
	}
}

func TestOpen_fragmentSiblingSelfImport(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "Alphasfile.f" { modules = ["a"] }`,
		"Alphasfile.f": `
module "a" {
  import "Alphasfile.f" { modules = ["s"] }

  service "go" "a" {
    git { url = "github.com/x/a" }
    vars = { p = module.s.service.go.s.vars.port }
  }
}

module "s" {
  service "go" "s" {
    git { url = "github.com/x/s" }
    vars = { port = 7000 }
  }
}
`,
	})
	af := openTree(t, root)
	if got := serviceNames(af); len(got) != 2 {
		t.Fatalf("services = %v, want a/a and s/s", got)
	}
	if got := fmt.Sprint(svcByName(af, "a/a").Runtime.Vars["p"]); got != "7000" {
		t.Errorf("a's p = %v", got)
	}
}

func TestOpen_entrypointModuleImport(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `
module "gw" {
  import "Alphasfile.b" { modules = ["b"] }

  service "go" "gw" {
    git { url = "github.com/x/gw" }
    vars = { upstream = module.b.service.go.b.name }
  }
}
`,
		"Alphasfile.b": `
module "b" {
  service "go" "b" {
    git { url = "github.com/x/b" }
  }
}
`,
	})
	af := openTree(t, root)
	if got := serviceNames(af); len(got) != 2 {
		t.Fatalf("services = %v, want gw/gw and b/b", got)
	}
}

func TestOpen_entrypointModuleImportNotVisibleAtTopLevel(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `
module "gw" {
  import "Alphasfile.b" { modules = ["b"] }
}

service "go" "z" {
  git { url = "github.com/x/z" }
  vars = { b = module.b.service.go.b.name }
}
`,
		"Alphasfile.b": `
module "b" {
  service "go" "b" {
    git { url = "github.com/x/b" }
  }
}
`,
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `add import "Alphasfile.b" { modules = ["b"] } at the top level`) {
		t.Fatalf("module gw's import must not leak to the top level, got %v", err)
	}
	if !strings.Contains(err.Error(), "the top level of "+filepath.Clean(root)) {
		t.Errorf("error must name the entrypoint: %v", err)
	}
}

func moduleNames(t *Tree) []string {
	out := make([]string, len(t.modules))
	for i, mb := range t.modules {
		out[i] = mb.Name
	}
	return out
}
