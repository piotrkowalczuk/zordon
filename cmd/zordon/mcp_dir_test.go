package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterbourgon/ff/v4"
)

// A bad --dir must fail before the server starts, so the client sees why
// rather than a server that silently resolves the wrong project. A nonexistent
// dir also lets the test exercise the flag without blocking on a live server.
func TestMCP_dirInvalid(t *testing.T) {
	root, _ := buildRootCommand(commandIO{Stdout: io.Discard, Stderr: io.Discard})
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	err := root.ParseAndRun(context.Background(), []string{"mcp", "--dir", missing}, ff.WithEnvVarPrefix("ZORDON"))
	if err == nil {
		t.Fatal("mcp --dir <missing> returned nil; want a chdir error")
	}
	if !strings.Contains(err.Error(), "mcp --dir") {
		t.Errorf("error = %q; want it to mention 'mcp --dir'", err)
	}
}
