package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// TestResolvePkgToolchain_toolsMap: `toolchain { pkg { tools } }` folds into
// the ToolchainConfig keyed ToolchainPkg with an empty Version, carrying the
// mise-ref → version map verbatim (the ref keeps its backend qualifier).
func TestResolvePkgToolchain_toolsMap(t *testing.T) {
	af := compile(t, `
toolchain {
  pkg {
    tools = {
      "aqua:ariga/atlas" = "1.2.0"
    }
  }
}
`, nil)
	tc := af.Toolchain[ToolchainPkg]
	if tc == nil {
		t.Fatal("toolchain[pkg] not resolved")
	}
	if tc.Version != "" {
		t.Errorf("pkg pseudo-toolchain must have no version, got %q", tc.Version)
	}
	if got := tc.Tools["aqua:ariga/atlas"]; got != "1.2.0" {
		t.Errorf("Tools[aqua:ariga/atlas] = %q, want 1.2.0", got)
	}
}

// TestResolvePkgToolchain_binRefResolves: a provision that env::prepends
// fs::toolchain::bin(toolchain.pkg) yields a prepend directive wrapping the
// "tc"/"pkg" BinSentinel — the exact chain alpha substitutes at run time.
func TestResolvePkgToolchain_binRefResolves(t *testing.T) {
	af := compile(t, `
toolchain {
  pkg {
    tools = { "aqua:ariga/atlas" = "1.2.0" }
  }
}

service "go" "app" {
  git { url = "github.com/acme/app" }

  runtime {
    cmd = ["app"]
    provision "migrate" {
      env = { PATH = env::prepend(fs::toolchain::bin(toolchain.pkg)) }
      cmd = "atlas version"
    }
  }
}
`, nil)
	app := svcByName(af, "app")
	if app == nil || app.Runtime == nil || len(app.Runtime.Provision) != 1 {
		t.Fatalf("migrate provision not resolved: %+v", app)
	}
	migrate := app.Runtime.Provision[0]

	resolved := SubstituteBins(migrate.Env["PATH"], func(kind, ref string) []string {
		if kind == "tc" && ref == ToolchainPkg {
			return []string{"/install/atlas/bin"}
		}
		return nil
	})
	op, args, ok := ParseEnvOp(resolved)
	if !ok || op != "prepend" {
		t.Fatalf("PATH is not a prepend directive: %q (ok=%v op=%q)", migrate.Env["PATH"], ok, op)
	}
	if len(args) != 1 || args[0] != "/install/atlas/bin" {
		t.Fatalf("prepend args = %q, want [/install/atlas/bin]", args)
	}
}

func TestResolvePkgToolchain_requiresVersion(t *testing.T) {
	err := compileErr(t, `
toolchain {
  pkg {
    tools = { "aqua:ariga/atlas" = "" }
  }
}
`)
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("want version-required error, got %v", err)
	}
}

func TestResolvePkgToolchain_requiresTools(t *testing.T) {
	err := compileErr(t, `
toolchain {
  pkg {
    tools = {}
  }
}
`)
	if !strings.Contains(err.Error(), "tools is required") {
		t.Fatalf("want tools-required error, got %v", err)
	}
}

// TestExamplePkgToolsResolves is the regression oracle for examples/pkg_tools:
// the pkg pseudo-toolchain resolves, and the migrate provision's PATH env is a
// prepend directive wrapping the toolchain.pkg bin sentinel. Pure Compile — no
// mise install, no spawn.
func TestExamplePkgToolsResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/pkg_tools/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		StateDir: "/repo/examples/pkg_tools/workspaces/main",
	}
	af, err := Compile("/repo/examples/pkg_tools/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	tc := af.Toolchain[ToolchainPkg]
	if tc == nil || tc.Tools["aqua:ariga/atlas"] != "1.2.0" {
		t.Fatalf("toolchain[pkg].Tools = %+v, want {aqua:ariga/atlas: 1.2.0}", tc)
	}

	app := svcByName(af, "app")
	if app == nil || app.Runtime == nil || len(app.Runtime.Provision) != 1 {
		t.Fatalf("app migrate provision not resolved: %+v", app)
	}
	migrate := app.Runtime.Provision[0]
	if migrate.Name != "migrate" {
		t.Fatalf("provision name = %q, want migrate", migrate.Name)
	}
	// App must wait for the migration before starting.
	if len(app.Runtime.After) == 0 || !strings.Contains(app.Runtime.After[0], "provision.migrate") {
		t.Errorf("runtime.after must gate on the migrate provision: %v", app.Runtime.After)
	}
	resolved := SubstituteBins(migrate.Env["PATH"], func(kind, ref string) []string {
		if kind == "tc" && ref == ToolchainPkg {
			return []string{"/install/atlas/bin"}
		}
		return nil
	})
	op, args, ok := ParseEnvOp(resolved)
	if !ok || op != "prepend" || len(args) != 1 || args[0] != "/install/atlas/bin" {
		t.Fatalf("migrate PATH not a prepend of the pkg bin dir: %q (ok=%v op=%q args=%q)",
			migrate.Env["PATH"], ok, op, args)
	}
}
