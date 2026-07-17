package zfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestZordonHome_overrideWins(t *testing.T) {
	if got := ZordonHome("/from/flag"); got != "/from/flag" {
		t.Errorf("ZordonHome(/from/flag) = %q; want explicit override to win", got)
	}
}

func TestZordonHome_defaultsToUserHomeZordon(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir unavailable on this platform")
	}
	want := filepath.Join(home, ".zordon")
	if got := ZordonHome(""); got != want {
		t.Errorf("ZordonHome(\"\") = %q; want %q", got, want)
	}
}

func TestFileDescriptor_setAndValidate(t *testing.T) {
	cases := map[string]struct {
		in      string
		want    int
		wantErr bool
	}{
		"empty":   {"", 0, false},
		"zero":    {"0", 0, false},
		"valid":   {"3", 3, false},
		"high":    {"99", 99, false},
		"stdin":   {"0", 0, false},
		"stdout":  {"1", 0, true},
		"stderr":  {"2", 0, true},
		"neg":     {"-1", 0, true},
		"garbage": {"oops", 0, true},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			var fd FileDescriptor
			err := fd.Set(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) wanted error; got fd=%d", c.in, fd)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q): %v", c.in, err)
			}
			if int(fd) != c.want {
				t.Errorf("Set(%q) → %d; want %d", c.in, int(fd), c.want)
			}
		})
	}
}

func TestDirName_setTrimsWhitespace(t *testing.T) {
	var d DirName
	if err := d.Set("  /some/path  "); err != nil {
		t.Fatal(err)
	}
	if d.Path() != "/some/path" {
		t.Errorf("DirName=%q; want trimmed", d.Path())
	}
}

func TestResolver_AlphaLogFile_overrideWins(t *testing.T) {
	got := NewResolver("/ws/a", "deadbeefcafef00d").AlphaLogFile("/explicit/alpha.log")
	if got != "/explicit/alpha.log" {
		t.Errorf("AlphaLogFile(override) = %q; want the explicit path to win", got)
	}
}

func TestResolver_AlphaLogFile_fallbackIsHashNamespaced(t *testing.T) {
	a := NewResolver("/ws/a", "deadbeefcafef00d").AlphaLogFile("")
	want := filepath.Join(SystemTempDir(), "alpha-deadbeefcafef00d.log")
	if a != want {
		t.Errorf("AlphaLogFile(\"\") = %q; want %q", a, want)
	}

	// Different workspaces (different FsHash) must not collide on one
	// shared alpha.log — that's the whole point of the hash in the name.
	b := NewResolver("/ws/b", "0123456789abcdef").AlphaLogFile("")
	if a == b {
		t.Errorf("two workspaces share default alpha log %q — hash not in the name", a)
	}
}

func TestResolver_ToolBinDirs(t *testing.T) {
	sep := string(os.PathListSeparator)
	cases := map[string]struct {
		fullPATH string
		basePATH string
		want     []string
	}{
		"single prepended dir": {
			"/dd/installs/pg/bin" + sep + "/zordon/bin" + sep + "/usr/bin",
			"/zordon/bin" + sep + "/usr/bin",
			[]string{"/dd/installs/pg/bin"},
		},
		"multiple leading dirs": {
			"/pg/bin" + sep + "/pg/lib/bin" + sep + "/zordon/bin",
			"/zordon/bin",
			[]string{"/pg/bin", "/pg/lib/bin"},
		},
		"shared dir in base excluded": {
			"/pg/bin" + sep + "/zordon/bin" + sep + "/usr/bin",
			"/zordon/bin" + sep + "/usr/bin",
			[]string{"/pg/bin"},
		},
		"empty delta": {
			"/zordon/bin" + sep + "/usr/bin",
			"/zordon/bin" + sep + "/usr/bin",
			nil,
		},
		"empty base returns all": {
			"/a" + sep + "/b",
			"",
			[]string{"/a", "/b"},
		},
		"base dir mid-list stops the walk": {
			"/new" + sep + "/usr/bin" + sep + "/other-new",
			"/usr/bin",
			[]string{"/new"},
		},
		"empty full path": {
			"",
			"/usr/bin",
			nil,
		},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got := NewResolver("", "").ForToolchain(c.fullPATH, c.basePATH).ToolBinDirs()
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestResolver_ToolBinDirs_panicsWhenUnscoped(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when ToolBinDirs is called without ForToolchain")
		}
	}()
	_ = NewResolver("/ws", "abc").ToolBinDirs()
}

// CheckSpawnCwd is the spawn-time companion to ServiceCwd (issue #44): a
// missing working directory must be named explicitly, never surfaced as the
// os/exec fork/exec-against-the-toolchain-binary red herring — so the word
// "fork/exec" must never appear in its message.
func TestCheckSpawnCwd(t *testing.T) {
	tmp := t.TempDir()
	existing := tmp
	missing := filepath.Join(tmp, "gone")
	aFile := filepath.Join(tmp, "afile")
	if err := AtomicWrite(aFile, []byte("x")); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	cases := map[string]struct {
		dir     string
		exe     string
		wantErr bool
		want    []string // substrings the message must contain
		notWant []string // substrings the message must NOT contain
	}{
		"existing dir": {
			dir: existing, exe: "./cmd/foo", wantErr: false,
		},
		"missing dir with exe": {
			dir: missing, exe: "./cmd/foo", wantErr: true,
			want:    []string{missing, `"./cmd/foo"`, "does not exist"},
			notWant: []string{"fork/exec"},
		},
		"missing dir no exe": {
			dir: missing, exe: "", wantErr: true,
			want:    []string{missing, "does not exist"},
			notWant: []string{"fork/exec"},
		},
		"missing dir dot exe": {
			dir: missing, exe: ".", wantErr: true,
			want:    []string{missing, "does not exist"},
			notWant: []string{`"."`, "fork/exec"},
		},
		"empty dir short-circuits": {
			dir: "", exe: "./cmd/foo", wantErr: false,
		},
		"path is a file": {
			dir: aFile, exe: "x", wantErr: true,
			want: []string{aFile, "not a directory"},
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			err := CheckSpawnCwd(c.dir, c.exe)
			if c.wantErr && err == nil {
				t.Fatalf("CheckSpawnCwd(%q, %q) = nil, want error", c.dir, c.exe)
			}
			if !c.wantErr {
				if err != nil {
					t.Fatalf("CheckSpawnCwd(%q, %q) = %v, want nil", c.dir, c.exe, err)
				}
				return
			}
			msg := err.Error()
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q missing substring %q", msg, w)
				}
			}
			for _, nw := range c.notWant {
				if strings.Contains(msg, nw) {
					t.Errorf("error %q unexpectedly contains %q", msg, nw)
				}
			}
		})
	}
}
