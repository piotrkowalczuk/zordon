package zfs

import (
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	base := filepath.FromSlash("/proj/workspaces/feature")

	cases := map[string]struct {
		rel  string
		want string
		ok   bool
	}{
		"plain file":            {rel: "CLAUDE.md", want: "/proj/workspaces/feature/CLAUDE.md", ok: true},
		"nested file":           {rel: ".claude/settings.json", want: "/proj/workspaces/feature/.claude/settings.json", ok: true},
		"dot prefix normalized": {rel: "./CLAUDE.md", want: "/proj/workspaces/feature/CLAUDE.md", ok: true},
		"interior climb":        {rel: "a/../b", want: "/proj/workspaces/feature/b", ok: true},
		"escaping climb":        {rel: "../other/x", ok: false},
		"deep escape":           {rel: "../../../etc/passwd", ok: false},
		"absolute":              {rel: "/etc/passwd", ok: false},
		"empty":                 {rel: "", ok: false},
		"climb back to base":    {rel: "a/..", ok: true, want: "/proj/workspaces/feature"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, ok := Resolve(base, c.rel)
			if ok != c.ok {
				t.Fatalf("Resolve(%q, %q) ok = %v, want %v (path %q)", base, c.rel, ok, c.ok, got)
			}
			if !c.ok {
				return
			}
			if want := filepath.FromSlash(c.want); got != want {
				t.Errorf("Resolve(%q, %q) = %q, want %q", base, c.rel, got, want)
			}
		})
	}
}

func TestWithin(t *testing.T) {
	cases := map[string]struct {
		base, path string
		want       bool
	}{
		"same dir":        {base: "/a/b", path: "/a/b", want: true},
		"child":           {base: "/a/b", path: "/a/b/c", want: true},
		"deep child":      {base: "/a/b", path: "/a/b/c/d/e", want: true},
		"parent":          {base: "/a/b", path: "/a", want: false},
		"sibling":         {base: "/a/b", path: "/a/c", want: false},
		"prefix impostor": {base: "/a/b", path: "/a/bc", want: false},
		"unclean child":   {base: "/a/b", path: "/a/b/./c", want: true},
		"unclean escape":  {base: "/a/b", path: "/a/b/../c", want: false},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			base, path := filepath.FromSlash(c.base), filepath.FromSlash(c.path)
			if got := Within(base, path); got != c.want {
				t.Errorf("Within(%q, %q) = %v, want %v", base, path, got, c.want)
			}
		})
	}
}
