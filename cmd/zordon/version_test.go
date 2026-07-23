package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/peterbourgon/ff/v4"

	"github.com/piotrkowalczuk/zordon/internal/zversion"
)

func TestVersionRequested(t *testing.T) {
	cases := map[string]struct {
		args []string
		want bool
	}{
		"long flag":                  {args: []string{"--version"}, want: true},
		"short flag":                 {args: []string{"-V"}, want: true},
		"bare word":                  {args: []string{"version"}, want: true},
		"no args":                    {args: nil, want: false},
		"a subcommand":               {args: []string{"status"}, want: false},
		"lowercase -v is verbose":    {args: []string{"-v"}, want: false},
		"only honored in first slot": {args: []string{"start", "--version"}, want: false},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := versionRequested(c.args); got != c.want {
				t.Errorf("versionRequested(%q)=%v, want %v", c.args, got, c.want)
			}
		})
	}
}

func TestSkewWarning(t *testing.T) {
	cases := map[string]struct {
		self, peer string
		wantEmpty  bool
	}{
		"same build":              {self: "v0.18.0", peer: "v0.18.0", wantEmpty: true},
		"alpha too old to report": {self: "v0.18.0", peer: "", wantEmpty: true},
		"mismatched pair":         {self: "v0.18.0", peer: "v0.17.1", wantEmpty: false},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got := skewWarning(c.self, c.peer)
			if (got == "") != c.wantEmpty {
				t.Errorf("skewWarning(%q, %q)=%q, wantEmpty=%v", c.self, c.peer, got, c.wantEmpty)
			}
		})
	}
}

// TestMCPImplementation guards against the version regressing to a hardcoded
// literal, which is what it was before it read from zversion.
func TestMCPImplementation(t *testing.T) {
	impl := mcpImplementation()

	if want := zversion.Get().Version; impl.Version != want {
		t.Errorf("Version=%q, want %q", impl.Version, want)
	}
	if impl.Name != "zordon" {
		t.Errorf("Name=%q, want %q", impl.Name, "zordon")
	}
}

// TestBuildRootCommand_noExec pins the behavior --version had to preserve:
// the root command still has no Exec of its own, so a bare `zordon` and an
// unknown subcommand both fall through to help rather than doing something.
func TestBuildRootCommand_noExec(t *testing.T) {
	cases := map[string]struct {
		args []string
	}{
		"no args":            {args: nil},
		"unknown subcommand": {args: []string{"bogus"}},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root, _ := buildRootCommand(commandIO{Stdout: io.Discard, Stderr: io.Discard})

			err := root.ParseAndRun(context.Background(), c.args, ff.WithEnvVarPrefix("ZORDON"))
			if !errors.Is(err, ff.ErrNoExec) && !errors.Is(err, ff.ErrHelp) {
				t.Errorf("ParseAndRun(%q) = %v, want ErrNoExec or ErrHelp", c.args, err)
			}
		})
	}
}
