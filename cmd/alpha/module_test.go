package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

func TestPinnedToolchains_moduleKeys(t *testing.T) {
	got := pinnedToolchains(map[string]*alphasfile.ToolchainConfig{
		"go":         {Version: "1.27.0"},
		"legacy/go":  {Version: "1.22.0"},
		"legacy/pkg": {Tools: map[string]string{"aqua:x/y": "1.0"}},
		"pkg":        {Tools: map[string]string{"aqua:a/b": "2.0"}},
		"ruby":       {},
		"nil":        nil,
	})
	byKey := map[string]*toolchainCtx{}
	for _, tc := range got {
		byKey[tc.key] = tc
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"go", "legacy/go", "legacy/pkg", "pkg"}; !equalKeys(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	cases := map[string]struct {
		lang, version string
		pkg           bool
	}{
		"go":         {"go", "1.27.0", false},
		"legacy/go":  {"go", "1.22.0", false},
		"legacy/pkg": {alphasfile.ToolchainPkg, "", true},
		"pkg":        {alphasfile.ToolchainPkg, "", true},
	}
	for key, c := range cases {
		t.Run(key, func(t *testing.T) {
			tc := byKey[key]
			if tc.lang != c.lang || tc.version != c.version || (tc.pkgTools != nil) != c.pkg {
				t.Errorf("lang=%q version=%q pkgTools=%v", tc.lang, tc.version, tc.pkgTools)
			}
		})
	}
}

func TestAlphaState_AddToolchain_keyedByKey(t *testing.T) {
	s := &alphaState{}
	legacy := newToolchainCtx("go", "1.22.0", nil, nil)
	legacy.key = "legacy/go"
	s.addToolchain(legacy)
	s.addToolchain(newToolchainCtx("go", "1.27.0", nil, nil))

	if got := s.toolchains["legacy/go"]; got != legacy {
		t.Errorf("legacy/go = %+v", got)
	}
	if got := s.toolchains["go"]; got == nil || got.version != "1.27.0" {
		t.Errorf("an empty key falls back to lang: %+v", got)
	}
}

func TestToolchainKey_module(t *testing.T) {
	cases := map[string]struct {
		svc  *alphasfile.Service
		want string
	}{
		"unpinned falls back to label": {&alphasfile.Service{Toolchain: "go"}, "go"},
		"entrypoint pin":               {&alphasfile.Service{Toolchain: "go", ToolchainKey: "go"}, "go"},
		"module pin":                   {&alphasfile.Service{Toolchain: "go", Module: "legacy", ToolchainKey: "legacy/go"}, "legacy/go"},
		"pkg keys by tool name":        {&alphasfile.Service{Toolchain: "pkg", ToolchainKey: "legacy/pkg", Pkg: &alphasfile.PkgSpec{Name: "redis"}}, "redis"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := toolchainKey(c.svc); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestAlphaState_ResolveBarrier_module(t *testing.T) {
	s := &alphaState{}
	s.addService(newServiceCtx("payments/db", "go"))
	s.addService(newServiceCtx("db", "go"))
	legacy := newToolchainCtx("go", "1.22.0", nil, nil)
	legacy.key = "legacy/go"
	s.addToolchain(legacy)

	cases := map[string]struct {
		ref string
		ok  bool
	}{
		"module runtime":             {"module.payments.service.go.db.runtime@ready", true},
		"module build":               {"module.payments.service.go.db.build@success", true},
		"flat runtime":               {"service.go.db.runtime@ready", true},
		"module toolchain ref":       {"module.legacy.toolchain.go@ready", true},
		"module toolchain by key":    {"toolchain.legacy/go@ready", true},
		"unknown module":             {"module.auth.service.go.db.runtime@ready", false},
		"unpinned module toolchain":  {"module.auth.toolchain.go@ready", false},
		"module ref without service": {"module.payments.runtime@ready", false},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			bt, err := s.resolveBarrier(c.ref)
			if c.ok && (err != nil || bt == nil || bt.target == nil) {
				t.Fatalf("resolveBarrier(%q) = %+v, %v", c.ref, bt, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("resolveBarrier(%q) must fail", c.ref)
			}
		})
	}
}

// Toolchains that name the binary themselves (cargo install, go install)
// must still produce <bin>/<module>/<name> for a module service, because
// the default run path is BinDir joined with the bare service name.
func TestDefaultBuild_moduleService(t *testing.T) {
	const binDir = "/p/workspaces/main/bin/payments"
	cases := map[string]struct {
		svc      *alphasfile.Service
		contains []string
	}{
		"go workspace build": {
			svc:      moduleBuildSvc(alphasfile.ToolchainGo, &alphasfile.Package{Src: "/src"}),
			contains: []string{`-o "/p/workspaces/main/bin/payments/db"`},
		},
		"go use-only install": {
			svc:      moduleBuildSvc(alphasfile.ToolchainGo, &alphasfile.Package{Install: "example.com/cmd/db@v1"}),
			contains: []string{`GOBIN="/p/workspaces/main/bin/payments"`},
		},
		"rust workspace build": {
			svc: moduleBuildSvc(alphasfile.ToolchainRust, &alphasfile.Package{Src: "/src"}),
			contains: []string{
				`--root "/p/workspaces/main/cargo/payments"`,
				`CARGO_TARGET_DIR="/p/.zordon/cache/rust/target"`,
				`&& cp -f "/p/workspaces/main/cargo/payments"/bin/* "/p/workspaces/main/bin/payments"/`,
			},
		},
		"rust crate install": {
			svc: moduleBuildSvc(alphasfile.ToolchainRust, &alphasfile.Package{Install: "db", Version: "1.0.0"}),
			contains: []string{
				`--root "/p/workspaces/main/cargo/payments"`,
				`&& cp -f "/p/workspaces/main/cargo/payments"/bin/* "/p/workspaces/main/bin/payments"/`,
			},
		},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got := defaultBuild(c.svc, artifactName(c.svc), binDir, "/src")
			for _, want := range c.contains {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in\n%s", want, got)
				}
			}
		})
	}
}

func TestDefaultBuild_defaultModuleRustUnchanged(t *testing.T) {
	svc := &alphasfile.Service{
		Toolchain: alphasfile.ToolchainRust,
		Runtime:   &alphasfile.RuntimeConfig{Name: "db"},
		Package:   &alphasfile.Package{Toolchain: alphasfile.ToolchainRust, Src: "/src"},
	}
	got := defaultBuild(svc, artifactName(svc), "/p/workspaces/main/bin", "/src")
	want := `CARGO_TARGET_DIR="/p/.zordon/cache/rust/target" cargo install --path . --root "/p/workspaces/main" --locked --force`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestBuildCmd_moduleRunsBareArtifact(t *testing.T) {
	svc := moduleBuildSvc(alphasfile.ToolchainGo, &alphasfile.Package{Src: "/src"})
	svc.Runtime.BinDir = "/p/workspaces/main/bin/payments"
	cmd, err := buildCmd(svc, "/src", nil, nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/p/workspaces/main/bin/payments/db" {
		t.Errorf("Path = %q", cmd.Path)
	}
}

func TestImplicitRuntimeAfter_moduleToolchainRef(t *testing.T) {
	s := &alphaState{}
	legacy := newToolchainCtx("go", "1.22.0", nil, nil)
	legacy.key = "legacy/go"
	s.addToolchain(legacy)
	s.addToolchain(newToolchainCtx("go", "1.27.0", nil, nil))

	cases := map[string]struct {
		svc  *alphasfile.Service
		want string
	}{
		"module pin":            {&alphasfile.Service{Toolchain: "go", Module: "legacy", ToolchainKey: "legacy/go"}, "module.legacy.toolchain.go@ready"},
		"module on default pin": {&alphasfile.Service{Toolchain: "go", Module: "modern", ToolchainKey: "go"}, "toolchain.go@ready"},
		"top level":             {&alphasfile.Service{Toolchain: "go", ToolchainKey: "go"}, "toolchain.go@ready"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got := implicitRuntimeAfter(c.svc, s)
			if len(got) != 1 || got[0] != c.want {
				t.Fatalf("got %v, want [%s]", got, c.want)
			}
			if _, err := s.resolveBarrier(got[0]); err != nil {
				t.Errorf("implicit dep must resolve: %v", err)
			}
		})
	}
}

func TestNewServiceCtx_idFromDisplayName(t *testing.T) {
	cases := map[string]struct{ name, want string }{
		"flat":   {"db", "service.go.db"},
		"module": {"payments/db", "module.payments.service.go.db"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := newServiceCtx(c.name, "go").id; got != c.want {
				t.Errorf("id = %q, want %q", got, c.want)
			}
		})
	}
}

func moduleBuildSvc(tc string, pkg *alphasfile.Package) *alphasfile.Service {
	pkg.Toolchain = tc
	return &alphasfile.Service{
		Toolchain: tc,
		Module:    "payments",
		Runtime:   &alphasfile.RuntimeConfig{Name: "db"},
		Package:   pkg,
	}
}

func equalKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
