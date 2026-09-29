package main

import (
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

func TestImportLines_noImports(t *testing.T) {
	tree, err := alphasfile.ParseTree("Alphasfile", []byte(`component "a" {}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := importLines("# ", tree); got != "" {
		t.Errorf("got %q", got)
	}
	if got := importLines("# ", nil); got != "" {
		t.Errorf("nil tree: got %q", got)
	}
}
