package alphasfile

import (
	"fmt"
	"strings"
	"testing"
)

const pkgGreeter = `
package "greeter" {
  inputs = {
    greeting = { description = "How to greet.", type = string, default = "hello" }
    name     = { description = "Whom to greet.", type = string }
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
  inputs = { port = { description = "Port; null picks one.", type = number, default = null } }

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

func TestOpen_inputsReadServices(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         "service \"go\" \"db\" {\n  git { url = \"github.com/x/db\" }\n  vars = { name = \"pg\" }\n}\nimport \"./greeter\" { inputs = { name = service.go.db.vars.name } }\n",
		"greeter/Alphasfile": pkgGreeter,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "greeter/greeter/greeter").Runtime.Vars["text"]); got != "hello pg" {
		t.Errorf("text = %q; an input is evaluated with the services, so it may read one", got)
	}
}

func TestOpen_inputDefaultReadsTheOwnPackage(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" {}`,
		"web/Alphasfile": `
package "web" {
  inputs = { url = { description = "Where web listens.", type = string, default = "http://127.0.0.1:${module.web.service.go.web.vars.port}" } }

  module "web" {
    service "go" "web" {
      git { url = "github.com/x/web" }
      vars = { port = 8080 }
    }
    service "go" "probe" {
      git { url = "github.com/x/probe" }
      vars = { target = inputs.url }
    }
  }
}
`,
	})
	if got := fmt.Sprint(svcByName(openTree(t, root), "web/web/probe").Runtime.Vars["target"]); got != "http://127.0.0.1:8080" {
		t.Errorf("target = %q", got)
	}
}

func TestOpen_inputMustFitItsType(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" { inputs = { port = "eighty" } }`,
		"web/Alphasfile": "package \"web\" {\n  inputs = { port = { description = \"Port.\", type = number } }\n}\n",
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `input "port" of package web is not a number`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_inputErrors(t *testing.T) {
	cases := map[string]struct{ entry, want string }{
		"missing required": {`import "./greeter" {}`, `package greeter needs input "name"`},
		"unknown":          {`import "./greeter" { inputs = { name = "x", nope = 1 } }`, `input "nope" is not declared by package greeter (declared: greeting, name)`},
		"not an object":    {`import "./greeter" { inputs = "x" }`, "inputs must be an object"},
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

func TestLoadTree_inputDeclarationErrors(t *testing.T) {
	cases := map[string]struct{ inputs, want string }{
		"not a map":           {`["a"]`, `inputs of package "web" map each name to { description = "...", ... }`},
		"short form":          {`{ port = 8080 }`, `input "port": inputs of package "web" map each name to`},
		"no description":      {`{ port = { type = number } }`, `input "port" of package "web" needs a description`},
		"empty description":   {`{ port = { description = "", type = number } }`, `input "port" of package "web" needs a description`},
		"no type":             {`{ port = { description = "Port." } }`, `input "port" of package "web" needs a type`},
		"bad type":            {`{ port = { description = "Port.", type = any } }`, "type any is not supported"},
		"unknown field":       {`{ port = { description = "Port.", type = number, max = 1 } }`, `input "port" takes description, type, default, many, unique only`},
		"many with default":   {`{ r = { description = "R.", type = object({ a = string }), many = true, default = {} } }`, `input "r" has many = true, so it starts empty and takes no default`},
		"many not an object":  {`{ r = { description = "R.", type = string, many = true } }`, `input "r": an input with many = true takes entries from provide blocks, so its type must be object`},
		"unique without many": {`{ r = { description = "R.", type = object({ a = string }), unique = ["a"] } }`, `input "r": unique applies to an input with many = true`},
		"bad name":            {`{ "no spaces" = { description = "x", type = string } }`, `input of package "web": name it with letters`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":     `import "./web" {}`,
				"web/Alphasfile": "package \"web\" {\n  inputs = " + c.inputs + "\n}\n",
			})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_manyInputIsNotSet(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":     `import "./web" { inputs = { routes = {} } }`,
		"web/Alphasfile": "package \"web\" {\n  inputs = { routes = { description = \"Routes.\", type = object({ path = string }), many = true } }\n}\n",
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `input "routes" of package web has many = true, so importers add entries with provide "routes" {} instead of setting it`) {
		t.Fatalf("got %v", err)
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
		"app/Alphasfile":     "package \"app\" {\n  inputs = { who = { description = \"Whom to greet.\", type = string } }\n  import \"../greeter\" { inputs = { name = inputs.who } }\n}\n",
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

func TestOpen_entrypointInputConflictsWithAnImport(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         "import \"./greeter\" { inputs = { name = \"y\" } }\nimport \"./a\" {}\n",
		"a/Alphasfile":       "package \"a\" {\n  import \"../greeter\" { inputs = { name = \"x\" } }\n}\n",
		"greeter/Alphasfile": pkgGreeter,
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
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
