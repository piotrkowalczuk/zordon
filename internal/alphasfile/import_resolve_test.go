package alphasfile

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The canonical scenario: a, b and c live in separate files, a depends on b,
// the entrypoint composes a and c. b enters the stack through a.
func TestOpen_transitiveModuleChain(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile": "import \"a/Alphasfile.a\" { modules = [\"a\"] }\nimport \"c/Alphasfile.c\" { modules = [\"c\"] }\n",
		"a/Alphasfile.a": `
module "a" {
  import "../b/Alphasfile.b" { modules = ["b"] }

  service "go" "a" {
    git { url = "github.com/x/a" }
    vars = { upstream = module.b.service.go.b.vars.port }
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
		"c/Alphasfile.c": `
module "c" {
  service "go" "c" {
    git { url = "github.com/x/c" }
  }
}
`,
	})
	af := openTree(t, root)
	if got := serviceNames(af); len(got) != 3 {
		t.Fatalf("services = %v", got)
	}
	if got := fmt.Sprint(svcByName(af, "a/a").Runtime.Vars["upstream"]); got != "7000" {
		t.Errorf("a's upstream = %v", got)
	}
}

func TestOpen_visibilityViolation(t *testing.T) {
	cases := map[string]string{
		"vars":          `vars = { p = module.b.service.go.b.vars.port }`,
		"env":           `env = { P = "${module.b.service.go.b.vars.port}" }`,
		"runtime.after": `runtime { after = [module.b.service.go.b.runtime.ready] }`,
		"provision.cmd": "runtime {\n  provision \"p\" {\n    cmd = module.b.service.go.b.runtime.provision.seed\n  }\n}",
		"file.body":     "file \"f\" {\n  path = \"/tmp/f\"\n  body = \"${module.b.service.go.b.vars.port}\"\n}",
		"print":         `print = "b=${module.b.service.go.b.vars.port}"`,
		"toolchain ref": `runtime { after = [module.b.toolchain.go.ready] }`,
	}
	for hint, body := range cases {
		t.Run(hint, func(t *testing.T) {
			dir := t.TempDir()
			root := writeTree(t, dir, map[string]string{
				"Alphasfile": fmt.Sprintf(`
import "a/Alphasfile.a" { modules = ["a"] }
service "go" "z" {
  git { url = "github.com/x/z" }
  %s
}
`, body),
				"a/Alphasfile.a": `
module "a" {
  import "../b/Alphasfile.b" { modules = ["b"] }
}
`,
				"b/Alphasfile.b": `
module "b" {
  toolchain {
    go { version = "1.22.0" }
  }
  service "go" "b" {
    git { url = "github.com/x/b" }
    vars = { port = 7000 }
    runtime {
      provision "seed" {
        after = never
        cmd   = "true"
      }
    }
  }
}
`,
			})
			_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
			if err == nil {
				t.Fatal("the entrypoint does not import module b, so module.b must not resolve")
			}
			for _, want := range []string{root + ":", "module.b is not visible", `add import "b/Alphasfile.b" { modules = ["b"] }`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("missing %q in %v", want, err)
				}
			}
		})
	}
}

func TestOpen_visibilityOK_cycle(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "Alphasfile.a" { modules = ["a"] }`,
		"Alphasfile.a": `
module "a" {
  import "Alphasfile.b" { modules = ["b"] }

  service "go" "a" {
    git { url = "github.com/x/a" }
    vars = { port = 1 }
    env  = { PEER = "${module.b.service.go.b.vars.port}" }
  }
}
`,
		"Alphasfile.b": `
module "b" {
  import "Alphasfile.a" { modules = ["a"] }

  service "go" "b" {
    git { url = "github.com/x/b" }
    vars = { port = 2 }
    env  = { PEER = "${module.a.service.go.a.vars.port}" }
  }
}
`,
	})
	af := openTree(t, root)
	if got := svcByName(af, "a/a").Runtime.Env["PEER"]; got != "2" {
		t.Errorf("a's PEER = %v", got)
	}
	if got := svcByName(af, "b/b").Runtime.Env["PEER"]; got != "1" {
		t.Errorf("b's PEER = %v", got)
	}
}

func TestOpen_srcPathAnchoredToDeclaringFile(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile": `import "services/kafka/Alphasfile.kafka" { modules = ["kafka"] }`,
		"services/kafka/Alphasfile.kafka": `
module "kafka" {
  service "go" "kafka" {
    src {
      path = "../.."
      exe  = "./cmd/kafka"
    }
  }
}
`,
	})
	af := openTree(t, root)
	k := svcByName(af, "kafka/kafka")
	if k.Package.Src != dir {
		t.Errorf("Src = %q, want %q (anchored at the fragment, not the entrypoint)", k.Package.Src, dir)
	}
	if want := filepath.Join(dir, "cmd/kafka"); k.Runtime.Dir != want {
		t.Errorf("Dir = %q, want %q", k.Runtime.Dir, want)
	}
}

func TestOpen_sysenvUnionAcrossFiles(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   "sysenv = [\"HOME\", \"PATH\"]\nimport \"Alphasfile.f\" { modules = [\"m\"] }\n",
		"Alphasfile.f": "sysenv = [\"PATH\", \"TZ\"]\nmodule \"m\" {}\n",
	})
	af := openTree(t, root)
	if got := af.SysEnv; !equalStrs(got, []string{"HOME", "PATH", "TZ"}) {
		t.Errorf("SysEnv = %v", got)
	}
}

func TestOpen_moduleToolchainFromFragment(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": "toolchain {\n  go { version = \"1.27.0\" }\n}\nimport \"Alphasfile.legacy\" { modules = [\"legacy\"] }\n",
		"Alphasfile.legacy": `
module "legacy" {
  toolchain {
    go { version = "1.22.0" }
  }
  service "go" "billing" {
    git { url = "github.com/x/billing" }
  }
}
`,
	})
	af := openTree(t, root)
	if af.Toolchain["legacy/go"] == nil || af.Toolchain["legacy/go"].Version != "1.22.0" {
		t.Fatalf("toolchain = %v", toolchainKeys(af))
	}
	if got := svcByName(af, "legacy/billing").ToolchainKey; got != "legacy/go" {
		t.Errorf("ToolchainKey = %q", got)
	}
}

func TestOpen_unusedModuleNotInstantiated(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "Alphasfile.f" { modules = ["used"] }`,
		"Alphasfile.f": `
module "used" {
  service "go" "a" {
    git { url = "github.com/x/a" }
  }
}
module "spare" {
  service "go" "b" {
    git { url = "github.com/x/b" }
  }
}
`,
	})
	af := openTree(t, root)
	if got := serviceNames(af); len(got) != 1 || got[0] != "used/a" {
		t.Errorf("services = %v", got)
	}
}

func TestParseServices_followsImports(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile": "service \"go\" \"gw\" {\n  src { path = \".\" }\n}\nimport \"svc/Alphasfile.svc\" { modules = [\"svc\"] }\n",
		"svc/Alphasfile.svc": `
module "svc" {
  service "go" "api" {
    src { path = "." }
  }
}
`,
	})
	metas, err := ParseServices(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 || metas[1].Name != "svc/api" || metas[1].Package.Src != filepath.Join(dir, "svc") {
		t.Errorf("metas = %+v / %+v", metas, metas[len(metas)-1].Package)
	}
}

func openTree(t *testing.T, root string) *Alphasfile {
	t.Helper()
	af, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return af
}
