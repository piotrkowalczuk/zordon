package alphasfile

import "testing"

func TestServiceRef(t *testing.T) {
	cases := map[string]struct {
		module, tc, name string
		want             string
	}{
		"default module": {"", "go", "api", "service.go.api"},
		"module":         {"payments", "go", "api", "module.payments.service.go.api"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := ServiceRef(c.module, c.tc, c.name); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseServiceRef(t *testing.T) {
	cases := map[string]struct {
		id                     string
		module, tc, name, rest string
		ok                     bool
	}{
		"flat":                   {"service.go.api", "", "go", "api", "", true},
		"flat with rest":         {"service.go.api.runtime.provision.migrate", "", "go", "api", "runtime.provision.migrate", true},
		"flat with state":        {"service.go.api.runtime@ready", "", "go", "api", "runtime", true},
		"module":                 {"module.payments.service.go.api", "payments", "go", "api", "", true},
		"module with rest+state": {"module.payments.service.go.api.build@success", "payments", "go", "api", "build", true},
		"toolchain":              {"toolchain.go@ready", "", "", "", "", false},
		"module without service": {"module.payments.toolchain.go", "", "", "", "", false},
		"truncated":              {"service.go", "", "", "", "", false},
		"empty module":           {"module..service.go.api", "", "", "", "", false},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			module, tc, name, rest, ok := ParseServiceRef(c.id)
			if ok != c.ok || module != c.module || tc != c.tc || name != c.name || rest != c.rest {
				t.Errorf("got (%q,%q,%q,%q,%v), want (%q,%q,%q,%q,%v)", module, tc, name, rest, ok, c.module, c.tc, c.name, c.rest, c.ok)
			}
		})
	}
}

func TestDisplayName_roundTrip(t *testing.T) {
	cases := map[string]struct{ module, name, display string }{
		"flat":   {"", "api", "api"},
		"module": {"payments", "api", "payments/api"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := DisplayName(c.module, c.name); got != c.display {
				t.Errorf("DisplayName = %q, want %q", got, c.display)
			}
			m, n := SplitDisplayName(c.display)
			if m != c.module || n != c.name {
				t.Errorf("SplitDisplayName = (%q,%q), want (%q,%q)", m, n, c.module, c.name)
			}
		})
	}
}

func TestToolchainKey_roundTrip(t *testing.T) {
	cases := map[string]struct{ module, lang, key string }{
		"flat":   {"", "go", "go"},
		"module": {"legacy", "go", "legacy/go"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := ToolchainKey(c.module, c.lang); got != c.key {
				t.Errorf("ToolchainKey = %q, want %q", got, c.key)
			}
			if got := ToolchainLang(c.key); got != c.lang {
				t.Errorf("ToolchainLang = %q, want %q", got, c.lang)
			}
		})
	}
}
