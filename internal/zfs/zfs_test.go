package zfs

import (
	"os"
	"path/filepath"
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
