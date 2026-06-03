package main

import (
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

func svc(name string, after ...string) *alphasfile.Service {
	return &alphasfile.Service{
		Toolchain: "go",
		Runtime:   &alphasfile.RuntimeConfig{Name: name, After: after},
	}
}

func names(in []*alphasfile.Service) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.Name()
	}
	return out
}

func TestParsePicks(t *testing.T) {
	cases := map[string]struct {
		in   []string
		want []string
	}{
		"space-separated":     {[]string{"a", "b"}, []string{"a", "b"}},
		"comma-separated":     {[]string{"a,b"}, []string{"a", "b"}},
		"mixed":               {[]string{"a,b", "c"}, []string{"a", "b", "c"}},
		"dedup":               {[]string{"a", "a,b"}, []string{"a", "b"}},
		"whitespace stripped": {[]string{" a , b "}, []string{"a", "b"}},
		"empty tokens":        {[]string{",a,,"}, []string{"a"}},
		"none":                {nil, nil},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got := parsePicks(c.in)
			if !equalStrings(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestServiceNameFromBarrierRef(t *testing.T) {
	cases := map[string]struct {
		ref     string
		want    string
		present bool
	}{
		"runtime":         {"service.go.api.runtime@ready", "api", true},
		"build":           {"service.rust.tansu.build@ready", "tansu", true},
		"provision":       {"service.go.db.runtime.provision.migrate@ready", "db", true},
		"no state suffix": {"service.go.api.runtime", "api", true},
		"toolchain":       {"toolchain.go@ready", "", false},
		"bogus root":      {"foo.bar@ready", "", false},
		"truncated":       {"service.go", "", false},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, ok := serviceNameFromBarrierRef(c.ref)
			if ok != c.present || got != c.want {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, c.want, c.present)
			}
		})
	}
}

func TestPickServices_TransitiveAfter(t *testing.T) {
	all := []*alphasfile.Service{
		svc("a"),
		svc("b", "service.go.a.runtime@ready"),
		svc("c", "service.go.b.runtime@ready"),
		svc("d"),
	}
	got, err := pickServices(all, []string{"c"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []string{"a", "b", "c"}
	if !equalStrings(names(got), want) {
		t.Errorf("got %v, want %v (transitive deps pulled in, d omitted)", names(got), want)
	}
}

func TestPickServices_PreservesInputOrder(t *testing.T) {
	all := []*alphasfile.Service{svc("a"), svc("b"), svc("c")}
	got, err := pickServices(all, []string{"c", "a"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []string{"a", "c"}
	if !equalStrings(names(got), want) {
		t.Errorf("got %v, want %v (order should follow Alphasfile, not picks)", names(got), want)
	}
}

func TestPickServices_UnknownIsError(t *testing.T) {
	all := []*alphasfile.Service{svc("a"), svc("b")}
	_, err := pickServices(all, []string{"a", "nope"})
	if err == nil {
		t.Fatal("expected error for unknown service")
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "available") {
		t.Errorf("error should name the unknown pick and list options: %v", err)
	}
}

func TestPickServices_IgnoresExternalAfterRefs(t *testing.T) {
	all := []*alphasfile.Service{
		svc("a", "toolchain.go@ready", "service.go.parent.runtime@ready"),
	}
	got, err := pickServices(all, []string{"a"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != 1 || got[0].Name() != "a" {
		t.Errorf("got %v, want [a] (federation parent / toolchain refs must not look like local picks)", names(got))
	}
}

func TestPickServices_ProvisionAfter(t *testing.T) {
	all := []*alphasfile.Service{
		svc("db"),
		{Toolchain: "go", Runtime: &alphasfile.RuntimeConfig{
			Name: "api",
			Provision: []*alphasfile.ProvisionStep{{
				Name:  "migrate",
				After: []string{"service.go.db.runtime@ready"},
			}},
		}},
	}
	got, err := pickServices(all, []string{"api"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !equalStrings(names(got), []string{"db", "api"}) {
		t.Errorf("provision.after deps must be pulled in too; got %v", names(got))
	}
}

func equalStrings(a, b []string) bool {
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
