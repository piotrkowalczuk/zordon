// Package fmtskip guards a build-tooling invariant: `make fmt` must
// format the real source tree but never descend into .zordon, which
// holds the installed Go toolchain and vendored third-party source
// (tens of thousands of .go files). Raw `gofmt -w .` would rewrite all
// of them, so the fmt target prunes every .zordon directory.
package fmtskip

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// unformatted is valid Go that gofmt is guaranteed to rewrite.
const unformatted = "package p\nfunc  F()  int  {  return  1  }\n"

func repoMakefile(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// tests/fmtskip/fmt_test.go -> repo root is two levels up.
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	mk := filepath.Join(root, "Makefile")
	if _, err := os.Stat(mk); err != nil {
		t.Fatalf("Makefile not found at %s: %v", mk, err)
	}
	return mk
}

// TestFmtSkipsZordon runs the real `make fmt` recipe against a throwaway
// tree and asserts it formats a normal package while leaving a planted
// .zordon/*.go file byte-for-byte untouched.
func TestFmtSkipsZordon(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make not available: %v", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not available: %v", err)
	}
	makefile := repoMakefile(t)

	dir := t.TempDir()
	write := func(rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(unformatted), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fmtskiptest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	normal := write("pkg/bad.go")          // a real package: must be formatted
	zordon := write(".zordon/skip/bad.go") // under .zordon: must be skipped

	// Run the actual fmt target's recipe in the throwaway tree.
	cmd := exec.Command("make", "-f", makefile, "fmt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make fmt failed: %v\n%s", err, out)
	}

	normalAfter, err := os.ReadFile(normal)
	if err != nil {
		t.Fatal(err)
	}
	if string(normalAfter) == unformatted {
		t.Fatalf("make fmt did not format the normal package file %s — recipe may not have run gofmt", normal)
	}

	zordonAfter, err := os.ReadFile(zordon)
	if err != nil {
		t.Fatal(err)
	}
	if string(zordonAfter) != unformatted {
		t.Fatalf("make fmt rewrote a file under .zordon (%s); it must be pruned.\nwant: %q\ngot:  %q", zordon, unformatted, zordonAfter)
	}
}
