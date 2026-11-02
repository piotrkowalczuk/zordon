package source

import "testing"

func TestNewPrimary_dirWinsOverGit(t *testing.T) {
	dir := t.TempDir()
	p, err := NewPrimary("", "github.com/a/x", dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindDir || p.Repo != dir {
		t.Fatalf("got %+v, want the dir primary at %s", p, dir)
	}
}
