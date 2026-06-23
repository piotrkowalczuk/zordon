package alphasfile

import (
	"slices"
	"strings"
	"testing"
)

// renderFlags turns one argument group into argv tokens per the options.
// The matrix covers each convention a target CLI might want — Go-flag
// default, GNU long, space-separated, Windows slash/colon, glued short
// flags, empty prefix — plus the Ruby override and within-group key sort.
func TestRenderFlags_byOptions(t *testing.T) {
	addr := map[string]any{"addr": "127.0.0.1:9000"}
	tests := []struct {
		name string
		tc   string
		opts *ArgOptions
		grp  map[string]any
		want []string
	}{
		{"default single-dash equals", ToolchainGo, nil, addr, []string{"-addr=127.0.0.1:9000"}},
		{"double-dash equals", ToolchainGo, &ArgOptions{Prefix: new("--")}, addr, []string{"--addr=127.0.0.1:9000"}},
		{"double-dash space → two argv", ToolchainGo, &ArgOptions{Prefix: new("--"), Separator: new(" ")}, addr, []string{"--addr", "127.0.0.1:9000"}},
		{"windows slash colon", ToolchainGo, &ArgOptions{Prefix: new("/"), Separator: new(":")}, map[string]any{"out": "file"}, []string{"/out:file"}},
		{"glued empty separator", ToolchainGo, &ArgOptions{Prefix: new("-"), Separator: new("")}, map[string]any{"O": 2}, []string{"-O2"}},
		{"empty prefix", ToolchainGo, &ArgOptions{Prefix: new("")}, map[string]any{"if": "/dev/zero"}, []string{"if=/dev/zero"}},
		{"ruby forces space over explicit equals", ToolchainRuby, &ArgOptions{Prefix: new("-"), Separator: new("=")}, addr, []string{"-addr", "127.0.0.1:9000"}},
		{"keys sorted within a group", ToolchainGo, nil, map[string]any{"c": 3, "a": 1, "b": 2}, []string{"-a=1", "-b=2", "-c=3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderFlags(tt.grp, tt.opts, tt.tc); !slices.Equal(got, tt.want) {
				t.Errorf("renderFlags() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Flags() (auto-append, used when there is no explicit runtime.cmd) renders
// every group and concatenates them in group-name order, deterministically.
func TestFlags_autoAppendsAllGroupsNameSorted(t *testing.T) {
	svc := &Service{
		Toolchain: ToolchainGo,
		Runtime: &RuntimeConfig{
			Arguments: map[string]map[string]any{
				"serve":  {"port": 8080},
				"global": {"debug": true},
			},
			Options: &ArgOptions{Prefix: new("--")},
		},
	}
	want := []string{"--debug=true", "--port=8080"} // global before serve
	if got := svc.Flags(); !slices.Equal(got, want) {
		t.Fatalf("Flags() = %v, want %v", got, want)
	}
}

// `arguments { values = { <group> = {…} } }` resolves into grouped Arguments
// (reachable via self.arguments.values.<group>.<key>); tpl::render::flags in
// runtime.cmd renders + splices a group into the argv, flattened in place.
func TestResolveArguments_groupsAndCmdSplice(t *testing.T) {
	af := compile(t, `
service "go" "gw" {
  package = "example.com/gw@v0.0.0"
  vars = { port = net::pickport() }
  arguments {
    values = {
      global = { debug = true }
      serve  = { addr = "127.0.0.1:${self.vars.port}" }
    }
    options { prefix = "--" }
  }
  runtime {
    cmd = ["gw", tpl::render::flags("global"), "serve", tpl::render::flags("serve")]
  }
}
`, nil)
	svc := svcByName(af, "gw")
	if svc == nil {
		t.Fatal("service gw not resolved")
	}
	if g := svc.Runtime.Arguments["global"]; g == nil || g["debug"] != true {
		t.Fatalf("group global = %v, want {debug:true}", svc.Runtime.Arguments["global"])
	}
	addr, _ := svc.Runtime.Arguments["serve"]["addr"].(string)
	if !strings.HasPrefix(addr, "127.0.0.1:") || strings.Contains(addr, "${") {
		t.Fatalf("serve.addr = %q, want resolved 127.0.0.1:<port>", addr)
	}
	want := []string{"gw", "--debug=true", "serve", "--addr=" + addr}
	if !slices.Equal(svc.Runtime.Command, want) {
		t.Fatalf("runtime cmd = %v, want %v (splice/flatten)", svc.Runtime.Command, want)
	}
}

func TestResolveArguments_rejectsScalarGroup(t *testing.T) {
	err := compileErr(t, `
service "go" "x" {
  package = "example.com/x@v0.0.0"
  arguments { values = { a = 1 } }
}
`)
	if !strings.Contains(err.Error(), "group") {
		t.Fatalf("want group-shape error, got %v", err)
	}
}

func TestResolveArguments_rejectsUnknownGroupInCmd(t *testing.T) {
	err := compileErr(t, `
service "go" "x" {
  package = "example.com/x@v0.0.0"
  arguments { values = { main = { a = 1 } } }
  runtime { cmd = ["x", tpl::render::flags("nope")] }
}
`)
	if !strings.Contains(err.Error(), "no such argument group") {
		t.Fatalf("want unknown-group error, got %v", err)
	}
}

func TestResolveArguments_rejectsInvalidPrefix(t *testing.T) {
	err := compileErr(t, `
service "go" "x" {
  package = "example.com/x@v0.0.0"
  arguments {
    values = { main = { a = 1 } }
    options { prefix = "!!" }
  }
}
`)
	if !strings.Contains(err.Error(), "prefix") {
		t.Fatalf("want prefix-invalid error, got %v", err)
	}
}

func TestResolveArguments_rejectsInvalidSeparator(t *testing.T) {
	err := compileErr(t, `
service "go" "x" {
  package = "example.com/x@v0.0.0"
  arguments {
    values = { main = { a = 1 } }
    options { separator = "~" }
  }
}
`)
	if !strings.Contains(err.Error(), "separator") {
		t.Fatalf("want separator-invalid error, got %v", err)
	}
}
