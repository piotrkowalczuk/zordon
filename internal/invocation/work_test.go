package invocation

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestFindWorkFile(t *testing.T) {
	root := realTempDir(t)
	deep := filepath.Join(root, "a", "b", "c")
	if err := zfs.EnsureDir(deep); err != nil {
		t.Fatal(err)
	}
	if got, err := FindWorkFile(deep); err != nil || got != "" {
		t.Fatalf("no file: got %q, %v", got, err)
	}
	work := filepath.Join(root, "a", WorkFileName)
	writeFile(t, work)
	if got, err := FindWorkFile(deep); err != nil || got != work {
		t.Fatalf("got %q, %v; want %q", got, err, work)
	}
}

func TestFindWorkFile_moreThanOneIsAnError(t *testing.T) {
	root := realTempDir(t)
	deep := filepath.Join(root, "a", "b")
	if err := zfs.EnsureDir(deep); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, WorkFileName))
	writeFile(t, filepath.Join(root, "a", WorkFileName))
	_, err := FindWorkFile(deep)
	if err == nil || !strings.Contains(err.Error(), "found 2 zordon.work files above") || !strings.Contains(err.Error(), "keep exactly one") {
		t.Fatalf("got %v", err)
	}
}

func TestFindWorkFile_followsThePhysicalPath(t *testing.T) {
	root := realTempDir(t)
	real := filepath.Join(root, "real", "project")
	if err := zfs.EnsureDir(real); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "real", WorkFileName))
	link := filepath.Join(root, "link")
	if err := zfs.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := FindWorkFile(link)
	if err != nil || got != filepath.Join(root, "real", WorkFileName) {
		t.Fatalf("got %q, %v; the file above the symlink target applies", got, err)
	}
}

func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := zfs.AtomicWrite(path, []byte("")); err != nil {
		t.Fatal(err)
	}
}
