package zfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAtomicWriteKeepingMode covers the reason the function exists: zordon
// edits fragments of files it does not own, and AtomicWrite lands everything
// on filePerm. A config quietly demoted from 0644 to 0600 breaks whatever else
// on the machine reads it.
func TestAtomicWriteKeepingMode(t *testing.T) {
	cases := map[string]os.FileMode{
		"world readable":  0o644,
		"group writable":  0o664,
		"executable":      0o755,
		"already private": filePerm,
	}

	for hint, mode := range cases {
		t.Run(hint, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "conf")
			if err := AtomicWrite(path, []byte("before\n")); err != nil {
				t.Fatalf("AtomicWrite: %v", err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("Chmod: %v", err)
			}

			if err := AtomicWriteKeepingMode(path, []byte("after\n")); err != nil {
				t.Fatalf("AtomicWriteKeepingMode: %v", err)
			}

			fi, err := Stat(path)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if got := fi.Mode().Perm(); got != mode {
				t.Errorf("mode = %v, want %v (the mode the file already had)", got, mode)
			}
			b, err := Read(path)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if string(b) != "after\n" {
				t.Errorf("content = %q, want the new bytes", b)
			}
		})
	}
}

// TestAtomicWriteKeepingMode_newFile: with nothing to preserve, it must behave
// exactly like AtomicWrite rather than inventing a mode.
func TestAtomicWriteKeepingMode_newFile(t *testing.T) {
	dir := t.TempDir()
	kept := filepath.Join(dir, "kept")
	plain := filepath.Join(dir, "plain")

	if err := AtomicWriteKeepingMode(kept, []byte("x")); err != nil {
		t.Fatalf("AtomicWriteKeepingMode: %v", err)
	}
	if err := AtomicWrite(plain, []byte("x")); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}

	a, err := Stat(kept)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	b, err := Stat(plain)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if a.Mode().Perm() != b.Mode().Perm() {
		t.Errorf("new file mode = %v, want AtomicWrite's %v", a.Mode().Perm(), b.Mode().Perm())
	}
}
