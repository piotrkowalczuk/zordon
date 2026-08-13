package zfs

import (
	"os"
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

// TestResolve_isLexicalOnly pins the DIVISION OF LABOUR, not a shortcoming:
// Resolve and Within compare cleaned strings and never touch the filesystem, so
// a symlink inside the base that points out of it still resolves to ok=true.
//
// Seeing through links is EvalExisting's job, and callers that need it compose
// the two — cmd/zordon's workspace guard resolves before it compares, which is
// what stops a link becoming a way past every rule it enforces (see
// TestWorkspaceFilePath_symlinkEscapingTheWorkspace).
//
// Keeping the primitive lexical is deliberate: it is total, needs no I/O, and
// works on paths that do not exist. This test fails if that ever changes
// quietly, since anything composing it would then be resolving twice.
func TestResolve_isLexicalOnly(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()

	link := filepath.Join(base, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, ok := Resolve(base, "escape/x")
	if !ok {
		t.Fatalf("Resolve refused %q — the guard has become filesystem-aware; update this test and the docs", got)
	}
	if !Within(base, got) {
		t.Errorf("Within(%q, %q) = false, want the lexical answer", base, got)
	}
	// The point: the path lexically inside base leads outside it on disk.
	real, err := filepath.EvalSymlinks(filepath.Dir(got))
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if Within(base, real) {
		t.Errorf("the symlink target %q is inside %q — the fixture did not set up an escape", real, base)
	}
}

// TestWithin_isCaseSensitive is the other half of the lexical limitation, and
// the one that actually bites on macOS. Within compares byte-for-byte, so on a
// case-insensitive filesystem "SRC/app" and "src/app" name the same directory
// while comparing unequal — a guard keyed on one spelling does not see the
// other.
//
// Same reasoning as TestResolve_isLexicalOnly: these paths come from a
// manifest the developer wrote, not from an attacker, so the trade is
// deliberate. Written down because a limitation nobody recorded is one
// somebody later assumes away.
func TestWithin_isCaseSensitive(t *testing.T) {
	base := filepath.FromSlash("/proj/workspaces/feature/src")
	other := filepath.FromSlash("/proj/workspaces/feature/SRC/app")

	if Within(base, other) {
		t.Fatalf("Within(%q, %q) = true — the comparison has become case-insensitive; update this test and the docs", base, other)
	}

	// And confirm the filesystem really would treat them as one, so the note
	// is about a live hazard rather than a hypothetical.
	dir := t.TempDir()
	if err := EnsureDir(filepath.Join(dir, "src")); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if _, err := Stat(filepath.Join(dir, "SRC")); err == nil {
		t.Log("filesystem is case-insensitive here: SRC and src are the same directory, and Within cannot tell")
	} else {
		t.Log("filesystem is case-sensitive here: the mismatch is only reachable on macOS/Windows")
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
