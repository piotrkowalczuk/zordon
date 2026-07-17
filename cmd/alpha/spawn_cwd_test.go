package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// checkSpawnCwd is the guard that turns a missing build/runtime/provision
// working directory into an explicit diagnosis instead of the os/exec
// fork/exec-against-the-toolchain-binary red herring (issue #44). The key
// contracts: a missing dir names the dir (and the exe hint), and the word
// "fork/exec" never appears.
func TestCheckSpawnCwd(t *testing.T) {
	tmp := t.TempDir()
	existing := tmp
	missing := filepath.Join(tmp, "gone")
	aFile := filepath.Join(tmp, "afile")
	if err := zfs.AtomicWrite(aFile, []byte("x")); err != nil {
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
			err := checkSpawnCwd(c.dir, c.exe)
			if c.wantErr && err == nil {
				t.Fatalf("checkSpawnCwd(%q, %q) = nil, want error", c.dir, c.exe)
			}
			if !c.wantErr {
				if err != nil {
					t.Fatalf("checkSpawnCwd(%q, %q) = %v, want nil", c.dir, c.exe, err)
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
