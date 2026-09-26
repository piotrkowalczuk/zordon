package alphasfile

import (
	"fmt"
	"strings"
	"testing"
)

const pkgGreeter = `
input "greeting" { default = "hello" }
input "name" {}

service "go" "greeter" {
  git { url = "github.com/x/greeter" }
  vars = { text = "${input.greeting} ${input.name}" }
}
`

func TestOpen_inputsFromImportAndDefault(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         `import "./greeter" { inputs = { name = "zordon" } }`,
		"greeter/Alphasfile": pkgGreeter,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "greeter/greeter").Runtime.Vars["text"]); got != "hello zordon" {
		t.Errorf("text = %q", got)
	}
}

func TestOpen_inputOverridesDefault(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         `import "./greeter" { inputs = { name = "zordon", greeting = "hi" } }`,
		"greeter/Alphasfile": pkgGreeter,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "greeter/greeter").Runtime.Vars["text"]); got != "hi zordon" {
		t.Errorf("text = %q", got)
	}
}

func TestOpen_inputDefaultFromEnvironment(t *testing.T) {
	t.Setenv("ZORDON_TEST_INPUT_NAME", "from-env")
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./greeter" {}`,
		"greeter/Alphasfile": `
input "name" { default = os::env("ZORDON_TEST_INPUT_NAME") }

service "go" "greeter" {
  git { url = "github.com/x/greeter" }
  vars = { name = input.name }
}
`,
	})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "greeter/greeter").Runtime.Vars["name"]); got != "from-env" {
		t.Errorf("name = %q", got)
	}
}

func TestLoadTree_inputErrors(t *testing.T) {
	cases := map[string]struct{ entry, want string }{
		"missing required": {`import "./greeter" {}`, `package greeter needs input "name"`},
		"unknown":          {`import "./greeter" { inputs = { name = "x", nope = 1 } }`, `input "nope" is not declared by package greeter (declared: greeting, name)`},
		"not an object":    {`import "./greeter" { inputs = "x" }`, "inputs must be an object"},
		"require passes":   {"require \"./greeter\" { inputs = { name = \"x\" } }\n", "cannot pass inputs or features"},
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

func TestLoadTree_requiredPackageNeedsDefaults(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":         `import "./app" {}`,
		"app/Alphasfile":     `require "../greeter" {}`,
		"greeter/Alphasfile": pkgGreeter,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "instead of only requiring it") {
		t.Fatalf("got %v", err)
	}
}

func TestCompile_inputWithoutDefaultCannotRunAlone(t *testing.T) {
	_, err := Compile("Alphasfile", []byte(pkgGreeter), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `input "name" has no default, so this Alphasfile cannot run on its own`) {
		t.Fatalf("got %v", err)
	}
}
