package zenv

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestEnvironmentVariables_Join_laterWinsOnCollision(t *testing.T) {
	base := EnvironmentVariables{"PATH": "/zordon/bin", "KEEP": "1"}
	got := base.Join(
		EnvironmentVariables{"PATH": "/mise/bin"},
		EnvironmentVariables{"PATH": "/user/bin", "EXTRA": "yes"},
	)
	if got["PATH"] != "/user/bin" {
		t.Errorf("PATH precedence wrong: got %q, want last-overlay value", got["PATH"])
	}
	if got["KEEP"] != "1" {
		t.Errorf("untouched key dropped: %v", got)
	}
	if got["EXTRA"] != "yes" {
		t.Errorf("overlay-only key missing: %v", got)
	}
	if _, ok := base["EXTRA"]; ok {
		t.Errorf("Join mutated receiver: %v", base)
	}
}

func TestEnvironmentVariables_Join_skipsNilAndEmpty(t *testing.T) {
	got := EnvironmentVariables{"A": "1"}.Join(nil, EnvironmentVariables{}, nil)
	if len(got) != 1 || got["A"] != "1" {
		t.Errorf("nil/empty overlays should be no-ops: %v", got)
	}
}

func TestEnvironmentVariables_JoinFile_parsesDotenv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	body := strings.Join([]string{
		"# comment line",
		"",
		"DB_URL=postgres://localhost/x",
		"LEVEL=base",
	}, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := EnvironmentVariables{"LEVEL": "preset"}.JoinFile(p)
	if got["DB_URL"] != "postgres://localhost/x" {
		t.Errorf("DB_URL not loaded: %v", got)
	}
	// File overrides earlier in-memory value — same precedence rule as Join.
	if got["LEVEL"] != "base" {
		t.Errorf("file should override earlier value: got LEVEL=%q", got["LEVEL"])
	}
}

func TestEnvironmentVariables_JoinFile_missingFileSkipped(t *testing.T) {
	// Federation can name a dotenv that only exists in some checkouts;
	// absence must not abort a service spawn.
	got := EnvironmentVariables{"A": "1"}.JoinFile("/nonexistent/.env")
	if got["A"] != "1" {
		t.Errorf("missing file should be silent no-op: %v", got)
	}
}

func TestEnvironmentVariables_Slice_keyValueShape(t *testing.T) {
	got := EnvironmentVariables{"A": "1", "B": "two"}.Slice()
	sort.Strings(got)
	want := []string{"A=1", "B=two"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Slice shape wrong: got %v", got)
	}
}

func TestFromHost_emptyAllowDropsEverything(t *testing.T) {
	t.Setenv("ZENVTEST_LEAK", "yes")
	got := FromHost(nil)
	if len(got) != 0 {
		t.Errorf("nil allow should drop everything: %v", got)
	}
}

func TestFromHost_whitelistKeepsOnlyListed(t *testing.T) {
	t.Setenv("ZENVTEST_KEEP", "yes")
	t.Setenv("ZENVTEST_DROP", "leak")
	got := FromHost([]string{"ZENVTEST_KEEP"})
	if got["ZENVTEST_KEEP"] != "yes" {
		t.Errorf("whitelisted var dropped: %v", got)
	}
	if _, ok := got["ZENVTEST_DROP"]; ok {
		t.Errorf("non-whitelisted var leaked: %v", got)
	}
}

func TestLookup(t *testing.T) {
	t.Setenv("ZENVTEST_SET", "value")
	if v, ok := Lookup("ZENVTEST_SET"); !ok || v != "value" {
		t.Errorf("Lookup wrong: ok=%v v=%q", ok, v)
	}
	if _, ok := Lookup("ZENVTEST_UNSET_XYZ"); ok {
		t.Errorf("Lookup on unset key should be ok=false")
	}
}

func TestEnvironmentVariables_PrependPath(t *testing.T) {
	sep := string(zfs.PathListSeparator)
	cases := map[string]struct {
		base EnvironmentVariables
		key  string
		dirs []string
		want string
	}{
		"empty dirs is a no-op":      {EnvironmentVariables{"PATH": "/usr/bin"}, "PATH", nil, "/usr/bin"},
		"absent key set to dirs":     {EnvironmentVariables{}, "PATH", []string{"/pg/bin"}, "/pg/bin"},
		"empty value set to dirs":    {EnvironmentVariables{"PATH": ""}, "PATH", []string{"/pg/bin"}, "/pg/bin"},
		"prepended before existing":  {EnvironmentVariables{"PATH": "/usr/bin"}, "PATH", []string{"/pg/bin"}, "/pg/bin" + sep + "/usr/bin"},
		"multiple dirs keep order":   {EnvironmentVariables{"PATH": "/usr/bin"}, "PATH", []string{"/pg/bin", "/redis/bin"}, "/pg/bin" + sep + "/redis/bin" + sep + "/usr/bin"},
		"dedup and drop empty entry": {EnvironmentVariables{"PATH": "/usr/bin"}, "PATH", []string{"/pg/bin", "", "/pg/bin"}, "/pg/bin" + sep + "/usr/bin"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := c.base.PrependPath(c.key, c.dirs); got[c.key] != c.want {
				t.Errorf("got %q, want %q", got[c.key], c.want)
			}
		})
	}
}

func TestEnvironmentVariables_AppendPath(t *testing.T) {
	sep := string(zfs.PathListSeparator)
	cases := map[string]struct {
		base EnvironmentVariables
		key  string
		dirs []string
		want string
	}{
		"empty dirs is a no-op":    {EnvironmentVariables{"PATH": "/usr/bin"}, "PATH", nil, "/usr/bin"},
		"absent key set to dirs":   {EnvironmentVariables{}, "PATH", []string{"/pg/bin"}, "/pg/bin"},
		"appended after existing":  {EnvironmentVariables{"PATH": "/usr/bin"}, "PATH", []string{"/pg/bin"}, "/usr/bin" + sep + "/pg/bin"},
		"multiple dirs keep order": {EnvironmentVariables{"PATH": "/usr/bin"}, "PATH", []string{"/pg/bin", "/redis/bin"}, "/usr/bin" + sep + "/pg/bin" + sep + "/redis/bin"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := c.base.AppendPath(c.key, c.dirs); got[c.key] != c.want {
				t.Errorf("got %q, want %q", got[c.key], c.want)
			}
		})
	}
}

func TestEnvironmentVariables_PrependPath_doesNotMutateReceiver(t *testing.T) {
	base := EnvironmentVariables{"PATH": "/usr/bin"}
	_ = base.PrependPath("PATH", []string{"/pg/bin"})
	if base["PATH"] != "/usr/bin" {
		t.Errorf("PrependPath mutated receiver: %q", base["PATH"])
	}
}
