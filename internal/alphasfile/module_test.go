package alphasfile

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompile_moduleServiceIdentity(t *testing.T) {
	af := compile(t, `
service "go" "gateway" {
  git { url = "github.com/x/gw" }
}
module "payments" {
  service "go" "db" {
    git { url = "github.com/x/db" }
    vars = { etc = fs::etc(), var = fs::var(), me = self.module }
  }
}
`, nil)
	db := svcByName(af, "payments/db")
	if db == nil {
		t.Fatalf("payments/db not resolved; have %v", serviceNames(af))
	}
	if db.Module != "payments" || db.Runtime.Name != "db" {
		t.Errorf("Module=%q Runtime.Name=%q; want payments / db", db.Module, db.Runtime.Name)
	}
	if got := db.ID(); got != "module.payments.service.go.db" {
		t.Errorf("ID() = %q", got)
	}
	if got := db.Runtime.Vars["etc"]; got != "/proj/workspaces/main/etc/payments/db" {
		t.Errorf("fs::etc() = %v", got)
	}
	if got := db.Runtime.Vars["var"]; got != "/proj/workspaces/main/var/payments/db" {
		t.Errorf("fs::var() = %v", got)
	}
	if got := db.Runtime.Vars["me"]; got != "payments" {
		t.Errorf("self.module = %v", got)
	}
	gw := svcByName(af, "gateway")
	if gw == nil || gw.Module != DefaultModule || gw.ID() != "service.go.gateway" {
		t.Errorf("top-level service keeps the flat identity: %+v", gw)
	}
}

func TestCompile_moduleShortRefResolvesWithinModule(t *testing.T) {
	af := compile(t, `
module "payments" {
  service "go" "db" {
    git { url = "github.com/x/db" }
    vars = { port = 5432 }
  }
  service "go" "api" {
    git { url = "github.com/x/api" }
    vars = { upstream = service.go.db.vars.port }
    runtime { after = [service.go.db.runtime.ready] }
  }
}
`, nil)
	api := svcByName(af, "payments/api")
	if got := fmt.Sprint(api.Runtime.Vars["upstream"]); got != "5432" {
		t.Errorf("service.go.db inside the module = %v; want 5432", got)
	}
	if got := api.Runtime.After; len(got) != 1 || got[0] != "module.payments.service.go.db.runtime@ready" {
		t.Errorf("after = %v", got)
	}
}

func TestCompile_crossModuleRefViaModuleRoot(t *testing.T) {
	af := compile(t, `
module "infra" {
  service "go" "kafka" {
    git { url = "github.com/x/kafka" }
    vars = { port = 9092 }
    runtime {
      provision "create-topic" {
        after = never
        cmd   = "echo $TOPIC"
      }
    }
  }
}
module "apps" {
  service "go" "app" {
    git { url = "github.com/x/app" }
    vars = { broker = "127.0.0.1:${module.infra.service.go.kafka.vars.port}" }
    runtime {
      after = [module.infra.service.go.kafka.runtime.ready]
      provision "topic" {
        cmd = module.infra.service.go.kafka.runtime.provision.create-topic
        env = { TOPIC = "app-events" }
      }
    }
  }
}
`, nil)
	app := svcByName(af, "apps/app")
	if got := app.Runtime.Vars["broker"]; got != "127.0.0.1:9092" {
		t.Errorf("broker = %v", got)
	}
	if got := app.Runtime.After; len(got) != 1 || got[0] != "module.infra.service.go.kafka.runtime@ready" {
		t.Errorf("after = %v", got)
	}
	topic := provByName(app, "topic")
	if topic == nil || topic.CmdRef != "module.infra.service.go.kafka.runtime.provision.create-topic" {
		t.Errorf("provision cmd-ref across modules: %+v", topic)
	}
}

func TestCompile_defaultModuleReferencesModule(t *testing.T) {
	af := compile(t, `
module "payments" {
  service "go" "api" {
    git { url = "github.com/x/api" }
    vars = { port = 8080 }
  }
}
service "go" "gateway" {
  git { url = "github.com/x/gw" }
  vars = { upstream = module.payments.service.go.api.vars.port }
}
`, nil)
	gw := svcByName(af, "gateway")
	if got := fmt.Sprint(gw.Runtime.Vars["upstream"]); got != "8080" {
		t.Errorf("upstream = %v", got)
	}
}

func TestCompile_shortRefDoesNotLeakAcrossModules(t *testing.T) {
	err := compileErr(t, `
module "infra" {
  service "go" "db" {
    git { url = "github.com/x/db" }
    vars = { port = 5432 }
  }
}
module "apps" {
  service "go" "api" {
    git { url = "github.com/x/api" }
    vars = { upstream = service.go.db.vars.port }
  }
}
`)
	if err == nil {
		t.Fatal("a bare service.go.db inside module apps must not resolve infra's db")
	}
}

func TestCompile_sameNameInTwoModulesIsNotDuplicate(t *testing.T) {
	af := compile(t, `
module "payments" {
  service "go" "db" {
    git { url = "github.com/x/db" }
  }
}
module "auth" {
  service "go" "db" {
    git { url = "github.com/x/db" }
  }
}
`, nil)
	if len(af.Services) != 2 {
		t.Fatalf("want 2 services, got %v", serviceNames(af))
	}
	if svcByName(af, "payments/db") == nil || svcByName(af, "auth/db") == nil {
		t.Errorf("names = %v", serviceNames(af))
	}
	if af.Services[0].ID() == af.Services[1].ID() {
		t.Errorf("ids must differ, both %q", af.Services[0].ID())
	}
}

func TestCompile_duplicateServiceInOneModule(t *testing.T) {
	err := compileErr(t, `
module "payments" {
  service "go" "db" {
    git { url = "github.com/x/db" }
  }
  service "go" "db" {
    git { url = "github.com/x/db2" }
  }
}
`)
	if err == nil || !strings.Contains(err.Error(), "duplicate service") || !strings.Contains(err.Error(), "payments/db") {
		t.Fatalf("want duplicate service naming payments/db, got %v", err)
	}
}

func TestCompile_serviceNameWithSlashRejected(t *testing.T) {
	err := compileErr(t, `
service "go" "payments/db" {
  git { url = "github.com/x/db" }
}
`)
	if err == nil || !strings.Contains(err.Error(), "invalid service name") || !strings.Contains(err.Error(), "payments/db") {
		t.Fatalf("got %v", err)
	}
}

func TestCompile_duplicateModuleName(t *testing.T) {
	err := compileErr(t, `
module "payments" {}
module "payments" {}
`)
	if err == nil || !strings.Contains(err.Error(), `duplicate module "payments"`) {
		t.Fatalf("got %v", err)
	}
}

func TestCompile_invalidModuleName(t *testing.T) {
	cases := map[string]string{
		"slash":         "pay/ments",
		"dot":           "pay.ments",
		"leading digit": "1payments",
		"empty":         "",
	}
	for hint, name := range cases {
		t.Run(hint, func(t *testing.T) {
			err := compileErr(t, fmt.Sprintf("module %q {}\n", name))
			if err == nil || !strings.Contains(err.Error(), "invalid module name") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestCompile_moduleToolchainKeyAndFallback(t *testing.T) {
	af := compile(t, `
toolchain {
  go { version = "1.27.0" }
}

service "go" "gateway" {
  git { url = "github.com/x/gw" }
}

module "legacy" {
  toolchain {
    go { version = "1.22.0" }
  }
  service "go" "billing" {
    git { url = "github.com/x/billing" }
  }
}
module "modern" {
  service "go" "api" {
    git { url = "github.com/x/api" }
  }
}
`, nil)
	if len(af.Toolchain) != 2 || af.Toolchain["go"] == nil || af.Toolchain["legacy/go"] == nil {
		t.Fatalf("toolchain keys = %v", toolchainKeys(af))
	}
	if af.Toolchain["legacy/go"].Version != "1.22.0" || af.Toolchain["go"].Version != "1.27.0" {
		t.Errorf("versions: %v / %v", af.Toolchain["legacy/go"].Version, af.Toolchain["go"].Version)
	}
	cases := map[string]string{"gateway": "go", "legacy/billing": "legacy/go", "modern/api": "go"}
	for name, want := range cases {
		if got := svcByName(af, name).ToolchainKey; got != want {
			t.Errorf("%s ToolchainKey = %q, want %q", name, got, want)
		}
	}
}

func TestCompile_moduleToolchainReadyBarrierRef(t *testing.T) {
	af := compile(t, `
toolchain {
  go { version = "1.27.0" }
}

service "go" "gateway" {
  git { url = "github.com/x/gw" }
  runtime { after = [module.legacy.toolchain.go.ready] }
}
module "legacy" {
  toolchain {
    go { version = "1.22.0" }
  }
  service "go" "billing" {
    git { url = "github.com/x/billing" }
    runtime { after = [toolchain.go.ready] }
  }
}
module "modern" {
  service "go" "api" {
    git { url = "github.com/x/api" }
    runtime { after = [toolchain.go.ready] }
  }
}
`, nil)
	cases := map[string]string{
		"legacy/billing": "toolchain.legacy/go@ready",
		"modern/api":     "toolchain.go@ready",
		"gateway":        "toolchain.legacy/go@ready",
	}
	for name, want := range cases {
		if got := svcByName(af, name).Runtime.After; len(got) != 1 || got[0] != want {
			t.Errorf("%s after = %v, want [%s]", name, got, want)
		}
	}
}

func TestCompile_moduleToolchainWithoutVersionFails(t *testing.T) {
	err := compileErr(t, `
module "legacy" {
  toolchain {
    go {}
  }
  service "go" "billing" {
    git { url = "github.com/x/billing" }
  }
}
`)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("got %v", err)
	}
}

func TestCompile_unpinnedServiceHasNoToolchainKey(t *testing.T) {
	af := compile(t, `
module "m" {
  service "go" "a" {
    git { url = "github.com/x/a" }
  }
}
`, nil)
	if got := svcByName(af, "m/a").ToolchainKey; got != "" {
		t.Errorf("ToolchainKey = %q, want empty", got)
	}
}

func TestCompile_moduleServiceBinAndEtcRefs(t *testing.T) {
	af := compile(t, `
module "payments" {
  service "go" "db" {
    git { url = "github.com/x/db" }
  }
}
service "go" "gateway" {
  git { url = "github.com/x/gw" }
  env = {
    DB_ETC = fs::service::etc(module.payments.service.go.db)
    DB_BIN = fs::service::bin(module.payments.service.go.db)
  }
}
`, nil)
	gw := svcByName(af, "gateway")
	if got := gw.Runtime.Env["DB_ETC"]; got != "/proj/workspaces/main/etc/payments/db" {
		t.Errorf("DB_ETC = %q", got)
	}
	if got, want := gw.Runtime.Env["DB_BIN"], BinSentinel("svc", "module.payments.service.go.db"); got != want {
		t.Errorf("DB_BIN = %q, want %q", got, want)
	}
}

func TestManifestState_Plan_moduleCycleIsPlanningError(t *testing.T) {
	src := `
module "a" {
  service "go" "x" {
    git { url = "github.com/x/x" }
    vars = { v = module.b.service.go.y.vars.v }
  }
}
module "b" {
  service "go" "y" {
    git { url = "github.com/x/y" }
    vars = { v = module.a.service.go.x.vars.v }
  }
}
`
	m, err := NewManifestState("cycle.hcl", []byte(src), testInv())
	if err != nil {
		t.Fatalf("parse must succeed: %v", err)
	}
	if _, err := m.Plan(nil, testCfgHash, TestConfig{}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Plan must report the cross-module vars cycle, got %v", err)
	}
}

func serviceNames(af *Alphasfile) []string {
	out := make([]string, 0, len(af.Services))
	for _, s := range af.Services {
		out = append(out, s.Name())
	}
	return out
}

func toolchainKeys(af *Alphasfile) []string {
	out := make([]string, 0, len(af.Toolchain))
	for k := range af.Toolchain {
		out = append(out, k)
	}
	return out
}
