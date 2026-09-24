package main

import (
	"sort"
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
		"module toolchain key":       {"toolchain.legacy/go@ready", true},
		"unknown module":             {"module.auth.service.go.db.runtime@ready", false},
		"unpinned toolchain key":     {"toolchain.auth/go@ready", false},
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
