package zordontest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// resolveFixturePath maps a path that's relative to the zordon repo
// root (e.g. "golden/go/echo") to an absolute path on disk.
//
// "Relative to repo root" is non-trivial: tests run with their package
// dir as CWD, which can be anywhere in the tree. We anchor on the
// CALLER's source file (via runtime.Caller) and walk up looking for
// a directory containing a top-level go.mod — that's the repo root.
// The walk-up stops at the filesystem root and fails the test if
// no go.mod was found, since the harness can't function without it.
func resolveFixturePath(t *testing.T, rel string) string {
	t.Helper()
	if filepath.IsAbs(rel) {
		return rel
	}
	root := repoRoot(t)
	return filepath.Join(root, rel)
}

// repoRoot finds the directory containing go.mod by walking up from
// this package's source file. Result is cached in a package var on
// first use — go.mod doesn't move during a test run.
func repoRoot(t *testing.T) string {
	t.Helper()
	if cachedRepoRoot != "" {
		return cachedRepoRoot
	}
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("repoRoot: runtime.Caller failed")
	}
	dir := filepath.Dir(here)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			cachedRepoRoot = dir
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repoRoot: no go.mod found above %s", here)
		}
		dir = parent
	}
}

var cachedRepoRoot string

// copyTree recursively copies srcRoot to dstRoot, preserving file
// modes. Symlinks are followed (their target's content is copied
// into a regular file at the destination).
//
// dstRoot's parent is created if missing; dstRoot itself MUST NOT
// exist (we don't want to silently merge into an existing tree —
// caller should remove or rename first if that's the intent).
func copyTree(srcRoot, dstRoot string) error {
	if _, err := os.Stat(dstRoot); err == nil {
		return fmt.Errorf("copyTree: destination %s already exists", dstRoot)
	}
	if err := os.MkdirAll(filepath.Dir(dstRoot), 0o755); err != nil {
		return err
	}
	return filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(dstRoot, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(dst, info.Mode())
		default:
			return copyFile(path, dst, info.Mode())
		}
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
