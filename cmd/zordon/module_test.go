package main

import (
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

func TestServiceNameFromBarrierRef_module(t *testing.T) {
	cases := map[string]struct {
		ref     string
		want    string
		present bool
	}{
		"module runtime":   {"module.payments.service.go.api.runtime@ready", "payments/api", true},
		"module build":     {"module.payments.service.go.api.build@success", "payments/api", true},
		"module provision": {"module.payments.service.go.db.runtime.provision.migrate@ready", "payments/db", true},
		"module toolchain": {"module.payments.toolchain.go@ready", "", false},
		"flat unchanged":   {"service.go.api.runtime@ready", "api", true},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, ok := serviceNameFromBarrierRef(c.ref)
			if ok != c.present || got != c.want {
				t.Errorf("got (%q,%v), want (%q,%v)", got, ok, c.want, c.present)
			}
		})
	}
}

func TestPickServices_modulePrefixedNames(t *testing.T) {
	all := []*alphasfile.Service{
		modSvc("payments", "db"),
		modSvc("payments", "api", "module.payments.service.go.db.runtime@ready"),
		modSvc("auth", "db"),
		svc("gateway", "module.payments.service.go.api.runtime@ready"),
	}
	got, err := pickServices(all, []string{"payments/api"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"payments/db", "payments/api"}; !equalStrings(names(got), want) {
		t.Errorf("picked %v, want %v", names(got), want)
	}
	if _, err := pickServices(all, []string{"api"}); err == nil || !strings.Contains(err.Error(), "payments/api") {
		t.Errorf("a bare name must not match a module service; available list should show payments/api, got %v", err)
	}
}

func TestRenderState_moduleBlocks(t *testing.T) {
	st := &protocol.StateInfo{
		Toolchain: map[string]*alphasfile.ToolchainConfig{
			"go":         {Version: "1.27.0"},
			"legacy/go":  {Version: "1.22.0"},
			"legacy/pkg": {Tools: map[string]string{"aqua:x/y": "1.0"}},
		},
		Services: []*alphasfile.Service{
			svc("gateway"),
			modSvc("legacy", "billing"),
			modSvc("auth", "db"),
		},
	}
	out := string(renderState(st))
	for _, want := range []string{
		"toolchain {\n  go {\n    version = \"1.27.0\"",
		"service \"go\" \"gateway\"",
		"module \"legacy\" {",
		"module \"auth\" {",
		"    version = \"1.22.0\"",
		"  service \"go\" \"billing\"",
		"  service \"go\" \"db\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Index(out, "service \"go\" \"gateway\"") > strings.Index(out, "module \"legacy\"") {
		t.Errorf("default module must render before module blocks\n%s", out)
	}
	if strings.Contains(out, "legacy/go") {
		t.Errorf("module pins render by language inside their module block, not by key\n%s", out)
	}
}

func TestRenderState_toolchainOnlyModule(t *testing.T) {
	out := string(renderState(&protocol.StateInfo{
		Toolchain: map[string]*alphasfile.ToolchainConfig{"tools/go": {Version: "1.22.0"}},
		Services:  []*alphasfile.Service{svc("api")},
	}))
	if !strings.Contains(out, "module \"tools\" {") || !strings.Contains(out, "version = \"1.22.0\"") {
		t.Errorf("a module that only pins a toolchain must still render\n%s", out)
	}
}

func TestRenderState_noModulesRendersNoModuleBlock(t *testing.T) {
	out := string(renderState(&protocol.StateInfo{Services: []*alphasfile.Service{svc("api")}}))
	if strings.Contains(out, "module") {
		t.Errorf("unexpected module block:\n%s", out)
	}
}

func TestBuildTree_moduleRoot(t *testing.T) {
	db := modSvc("payments", "db")
	db.Runtime.Vars = map[string]any{"port": 5432}
	gw := svc("gateway")
	gw.Runtime.Vars = map[string]any{"port": 8080}
	lv := &level{state: &protocol.StateInfo{
		Services: []*alphasfile.Service{db, gw},
		Running:  []protocol.ServiceStatus{{Name: "payments/db", PID: 42, Readiness: protocol.ReadinessReady}},
	}}
	root := buildTree([]*level{lv})

	cases := map[string]struct {
		path string
		want string
	}{
		"module var":      {"module.payments.service.go.db.vars.port", "5432"},
		"module running":  {"module.payments.service.go.db.running", "true"},
		"module pid":      {"module.payments.service.go.db.pid", "42"},
		"default var":     {"service.go.gateway.vars.port", "8080"},
		"default running": {"service.go.gateway.running", "false"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := resolveExpr(root, c.path)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("%s = %q, want %q", c.path, got, c.want)
			}
		})
	}
	if _, err := resolveExpr(root, "service.go.db.vars.port"); err == nil {
		t.Error("a module service must not be reachable under the flat service root")
	}
}

func modSvc(module, name string, after ...string) *alphasfile.Service {
	s := svc(name, after...)
	s.Module = module
	return s
}
