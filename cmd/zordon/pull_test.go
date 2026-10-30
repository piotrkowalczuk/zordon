package main

import (
	"strings"
	"testing"
)

func TestPullFlag_Set(t *testing.T) {
	cases := map[string]struct {
		in       string
		srv, pkg bool
	}{
		"bare":     {"true", true, true},
		"all":      {"all", true, true},
		"srv":      {"srv", true, false},
		"pkg":      {"pkg", false, true},
		"both":     {"srv,pkg", true, true},
		"spaced":   {"srv, pkg", true, true},
		"disabled": {"false", false, false},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			var p pullFlag
			if err := p.Set(c.in); err != nil {
				t.Fatal(err)
			}
			if p.srv != c.srv || p.pkg != c.pkg {
				t.Errorf("Set(%q) = %+v, want srv=%v pkg=%v", c.in, p, c.srv, c.pkg)
			}
		})
	}
}

func TestPullFlag_Set_rejectsAPolicy(t *testing.T) {
	var p pullFlag
	if err := p.Set("always"); err == nil || !strings.Contains(err.Error(), "--pull takes srv, pkg or all") {
		t.Fatalf("got %v", err)
	}
}

func TestPullFlag_Set_lastOneWins(t *testing.T) {
	var p pullFlag
	for _, v := range []string{"srv", "pkg"} {
		if err := p.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if p.srv || !p.pkg {
		t.Errorf("--pull=srv --pull=pkg = %+v, want pkg only", p)
	}
}
