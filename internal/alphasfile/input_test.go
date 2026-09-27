package alphasfile

import (
	"fmt"
	"strings"
	"testing"
)

const pkgGreeter = `
package "greeter" {
  inputs = {
    greeting = "hello"
    name     = required
  }

  module "greeter" {
    service "go" "greeter" {
      git { url = "github.com/x/greeter" }
      vars = { text = "${inputs.greeting} ${inputs.name}" }
    }
  }
}
`

func TestOpen_inputsFromImportAndDefault(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         `import "./greeter" { inputs = { name = "zordon" } }`,
		"greeter/Alphasfile": pkgGreeter,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "greeter/greeter/greeter").Runtime.Vars["text"]); got != "hello zordon" {
		t.Errorf("text = %q", got)
	}
}

func TestOpen_inputOverridesDefault(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         `import "./greeter" { inputs = { name = "zordon", greeting = "hi" } }`,
		"greeter/Alphasfile": pkgGreeter,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "greeter/greeter/greeter").Runtime.Vars["text"]); got != "hi zordon" {
		t.Errorf("text = %q", got)
	}
}

func TestOpen_inputNullIsAValue(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" {}`,
		"web/Alphasfile": `
package "web" {
  inputs = { port = null }

  module "web" {
    service "go" "web" {
      git { url = "github.com/x/web" }
      vars = { port = inputs.port != null ? inputs.port : 8080 }
    }
  }
}
`,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "web/web/web").Runtime.Vars["port"]); got != "8080" {
		t.Errorf("port = %s", got)
	}
}

func TestOpen_importerInputFromEnvironment(t *testing.T) {
	t.Setenv("ZORDON_TEST_INPUT_NAME", "from-env")
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         `import "./greeter" { inputs = { name = os::env("ZORDON_TEST_INPUT_NAME") } }`,
		"greeter/Alphasfile": pkgGreeter,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "greeter/greeter/greeter").Runtime.Vars["text"]); got != "hello from-env" {
		t.Errorf("text = %q", got)
	}
}

func TestLoadTree_inputErrors(t *testing.T) {
	cases := map[string]struct{ entry, want string }{
		"missing required": {`import "./greeter" {}`, `package greeter needs input "name"`},
		"unknown":          {`import "./greeter" { inputs = { name = "x", nope = 1 } }`, `input "nope" is not declared by package greeter (declared: greeting, name)`},
		"not an object":    {`import "./greeter" { inputs = "x" }`, "inputs must be an object"},
		"services refs":    {`import "./greeter" { inputs = { name = service.go.x.name } }`, "inputs:"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":         c.entry,
				"greeter/Alphasfile": pkgGreeter,
			})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_inputsCannotReadServices(t *testing.T) {
	cases := map[string]struct{ files map[string]string }{
		"import": {map[string]string{
			"Alphasfile":         "service \"go\" \"db\" {\n  git { url = \"github.com/x/db\" }\n}\nimport \"./greeter\" { inputs = { name = service.go.db.vars.port } }\n",
			"greeter/Alphasfile": pkgGreeter,
		}},
		"default": {map[string]string{
			"Alphasfile":     `import "./web" {}`,
			"web/Alphasfile": "package \"web\" {\n  inputs = { port = module.web.service.go.web.vars.port }\n  module \"web\" {\n    service \"go\" \"web\" {\n      git { url = \"github.com/x/web\" }\n    }\n  }\n}\n",
		}},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			_, err := LoadTree(writeTree(t, t.TempDir(), c.files))
			if err == nil || !strings.Contains(err.Error(), "inputs are known before planning, so they cannot read") || !strings.Contains(err.Error(), "provide it to a slot, or read an output") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestLoadTree_packageImportMustPassRequiredInput(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         `import "./app" {}`,
		"app/Alphasfile":     "package \"app\" {\n  import \"../greeter\" {}\n}\n",
		"greeter/Alphasfile": pkgGreeter,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "app/Alphasfile:2") || !strings.Contains(err.Error(), `package greeter needs input "name"; pass it with inputs = { name = ... }`) {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_packageImportConfiguresItsDependency(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         `import "./app" { inputs = { who = "zordon" } }`,
		"app/Alphasfile":     "package \"app\" {\n  inputs = { who = required }\n  import \"../greeter\" { inputs = { name = inputs.who } }\n}\n",
		"greeter/Alphasfile": pkgGreeter,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "greeter/greeter/greeter").Runtime.Vars["text"]); got != "hello zordon" {
		t.Errorf("text = %q; a package's import passes its own inputs on", got)
	}
}

func TestLoadTree_inputHasOneSource(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         "import \"./a\" {}\nimport \"./b\" {}\n",
		"a/Alphasfile":       "package \"a\" {\n  import \"../greeter\" { inputs = { name = \"x\" } }\n}\n",
		"b/Alphasfile":       "package \"b\" {\n  import \"../greeter\" { inputs = { name = \"x\" } }\n}\n",
		"greeter/Alphasfile": pkgGreeter,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), `package b sets input "name" of package greeter, but package a already sets it at`) || !strings.Contains(err.Error(), "an input has one source, so set it where the entrypoint imports package greeter") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen_entrypointSetsAnInputOthersAlsoSet(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         "import \"./greeter\" { inputs = { name = \"x\" } }\nimport \"./a\" {}\n",
		"a/Alphasfile":       "package \"a\" {\n  import \"../greeter\" { inputs = { name = \"x\" } }\n}\n",
		"greeter/Alphasfile": pkgGreeter,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "greeter/greeter/greeter").Runtime.Vars["text"]); got != "hello x" {
		t.Errorf("text = %q", got)
	}
}

func TestLoadTree_entrypointInputConflictsWithAnImport(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         "import \"./greeter\" { inputs = { name = \"y\" } }\nimport \"./a\" {}\n",
		"a/Alphasfile":       "package \"a\" {\n  import \"../greeter\" { inputs = { name = \"x\" } }\n}\n",
		"greeter/Alphasfile": pkgGreeter,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), `Alphasfile:1`) || !strings.Contains(err.Error(), `package greeter runs with input "name" = "y", but package a needs "x"`) || !strings.Contains(err.Error(), `set inputs = { name = "x" } here`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_requiredInputStopsARunOnItsOwn(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": pkgGreeter})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `input "name" of package greeter is required, so the package cannot run on its own`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_inputsMustBeAnObject(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" {}`,
		"web/Alphasfile": "package \"web\" {\n  inputs = [\"a\"]\n}\n",
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "inputs of package web must be an object such as { name = default }") {
		t.Fatalf("got %v", err)
	}
}
