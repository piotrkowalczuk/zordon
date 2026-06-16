package main

import (
	"io"
	"slices"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

// TestCommandToolSubcommands pins the MCP command surface to the CLI surface:
// every subcommand except `mcp` becomes a tool. If a command is added or
// removed, this fails so the author confirms the MCP surface intentionally.
func TestCommandToolSubcommands(t *testing.T) {
	root, _ := buildRootCommand(commandIO{Stdout: io.Discard, Stderr: io.Discard})

	var all []string
	for _, s := range root.Subcommands {
		all = append(all, s.Name)
	}
	slices.Sort(all)
	wantAll := []string{"clean", "get", "mcp", "plan", "start", "status", "stop", "sudo", "worktree"}
	if !slices.Equal(all, wantAll) {
		t.Fatalf("subcommands = %v; want %v (update the MCP surface intentionally)", all, wantAll)
	}

	var tools []string
	for _, s := range commandToolSubcommands(root) {
		tools = append(tools, s.Name)
	}
	slices.Sort(tools)
	wantTools := []string{"clean", "get", "plan", "start", "status", "stop", "sudo", "worktree"}
	if !slices.Equal(tools, wantTools) {
		t.Errorf("command tools = %v; want %v (mcp excluded)", tools, wantTools)
	}
}

func TestCommandArgv(t *testing.T) {
	cases := map[string]struct {
		home    string
		agent   bool
		testCfg alphasfile.TestConfig
		name    string
		args    []string
		want    []string
	}{
		"full root context": {
			home:    "/x",
			agent:   true,
			testCfg: alphasfile.TestConfig{Harness: true, LogPath: "/l"},
			name:    "status",
			args:    []string{"--foo"},
			want:    []string{"--home", "/x", "--agent", "--test-harness", "--test-log", "/l", "status", "--foo"},
		},
		"minimal": {
			name: "get",
			args: []string{"service.go.app.vars.port"},
			want: []string{"get", "service.go.app.vars.port"},
		},
		"home only": {
			home: "/home/.zordon",
			name: "plan",
			want: []string{"--home", "/home/.zordon", "plan"},
		},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got := commandArgv(c.home, c.agent, c.testCfg, c.name, c.args)
			if !slices.Equal(got, c.want) {
				t.Errorf("commandArgv = %v; want %v", got, c.want)
			}
		})
	}
}

func TestJoinOutput(t *testing.T) {
	cases := map[string]struct {
		parts []string
		want  string
	}{
		"skips empties":     {[]string{"", "a", ""}, "a"},
		"adds separator":    {[]string{"a", "b"}, "a\nb"},
		"respects trailing": {[]string{"a\n", "b"}, "a\nb"},
		"all empty":         {[]string{"", ""}, ""},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := joinOutput(c.parts...); got != c.want {
				t.Errorf("joinOutput(%q) = %q; want %q", c.parts, got, c.want)
			}
		})
	}
}
