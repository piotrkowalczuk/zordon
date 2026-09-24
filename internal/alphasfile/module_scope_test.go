package alphasfile

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestCompile_debuggerToolsGoToModulePin(t *testing.T) {
	af := compile(t, `
toolchain {
  go { version = "1.27.0" }
}
module "legacy" {
  toolchain {
    go { version = "1.22.0" }
  }
  service "go" "billing" {
    git { url = "github.com/x/billing" }
    debugger { enabled = true }
  }
}
`, nil)
	if _, ok := af.Toolchain["legacy/go"].Tools[debuggerToolDlv]; !ok {
		t.Errorf("dlv must land in the module's pin: %+v", af.Toolchain["legacy/go"].Tools)
	}
	if _, ok := af.Toolchain["go"].Tools[debuggerToolDlv]; ok {
		t.Errorf("the default pin must stay untouched: %+v", af.Toolchain["go"].Tools)
	}
}

func TestCompile_debuggerInModuleWithoutPinFails(t *testing.T) {
	err := compileErr(t, `
module "m" {
  service "go" "a" {
    git { url = "github.com/x/a" }
    debugger { enabled = true }
  }
}
`)
	if err == nil || !strings.Contains(err.Error(), "m/a") || !strings.Contains(err.Error(), "toolchain") {
		t.Fatalf("got %v", err)
	}
}

func TestCompile_modulePkgToolchainBinRef(t *testing.T) {
	af := compile(t, `
module "m" {
  toolchain {
    pkg {
      tools = { "aqua:ariga/atlas" = "0.29.0" }
    }
  }
  service "go" "api" {
    git { url = "github.com/x/api" }
    env = { ATLAS = fs::toolchain::bin(toolchain.pkg) }
  }
}
`, nil)
	if af.Toolchain["m/pkg"] == nil || af.Toolchain["pkg"] != nil {
		t.Fatalf("toolchain keys = %v", toolchainKeys(af))
	}
	if got, want := svcByName(af, "m/api").Runtime.Env["ATLAS"], BinSentinel("tc", "m/pkg"); got != want {
		t.Errorf("ATLAS = %q, want %q", got, want)
	}
}

func TestCompile_moduleProvisionCmdRefWithArgumentsRejected(t *testing.T) {
	err := compileErr(t, `
module "infra" {
  service "go" "db" {
    git { url = "github.com/x/db" }
    runtime {
      provision "seed" {
        argument "key" {
          type = "string"
        }
        after = never
        cmd   = "seed $key"
      }
    }
  }
}
service "go" "app" {
  git { url = "github.com/x/app" }
  runtime {
    provision "use" {
      cmd = module.infra.service.go.db.runtime.provision.seed
    }
  }
}
`)
	if err == nil || !strings.Contains(err.Error(), "declares arguments") {
		t.Fatalf("got %v", err)
	}
}

func TestCompile_federationParentModuleRef(t *testing.T) {
	parent := NewParentContext([]*Service{{
		Toolchain: "go",
		Module:    "infra",
		Runtime:   &RuntimeConfig{Name: "db", Vars: map[string]any{"port": int64(5432)}},
	}})
	af := compile(t, `
service "go" "app" {
  git { url = "github.com/x/app" }
  vars = { db = module.infra.service.go.db.vars.port }
}
`, parent)
	if got := fmt.Sprint(svcByName(af, "app").Runtime.Vars["db"]); got != "5432" {
		t.Errorf("db = %v", got)
	}
}

func TestCompile_federationParentModuleNotReachableBare(t *testing.T) {
	parent := NewParentContext([]*Service{{
		Toolchain: "go",
		Module:    "infra",
		Runtime:   &RuntimeConfig{Name: "db", Vars: map[string]any{"port": int64(5432)}},
	}})
	_, err := Compile("test.hcl", []byte(`
service "go" "app" {
  git { url = "github.com/x/app" }
  vars = { db = service.go.db.vars.port }
}
`), testInv(), parent, testCfgHash, TestConfig{})
	if err == nil {
		t.Fatal("a parent's module service must not resolve through a bare service.* ref")
	}
}

func TestParseServices_modules(t *testing.T) {
	dir := t.TempDir()
	afPath := filepath.Join(dir, "Alphasfile")
	body := `
service "go" "gateway" {
  src { path = "." }
}
module "payments" {
  service "go" "db" {
    src { path = "./db" }
  }
}
`
	if err := zfs.AtomicWrite(afPath, []byte(body)); err != nil {
		t.Fatal(err)
	}
	metas, err := ParseServices(afPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("got %d metas", len(metas))
	}
	db := metas[1]
	if db.Name != "payments/db" || db.Module != "payments" || db.Package.Src != filepath.Join(dir, "db") {
		t.Errorf("db meta = %+v / %+v", db, db.Package)
	}
	if metas[0].Name != "gateway" || metas[0].Module != DefaultModule {
		t.Errorf("gateway meta = %+v", metas[0])
	}
}

func TestParseServices_rejectsSlashInServiceName(t *testing.T) {
	dir := t.TempDir()
	afPath := filepath.Join(dir, "Alphasfile")
	if err := zfs.AtomicWrite(afPath, []byte("service \"go\" \"a/b\" {\n  src { path = \".\" }\n}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseServices(afPath); err == nil || !strings.Contains(err.Error(), "invalid service name") {
		t.Fatalf("got %v", err)
	}
}
