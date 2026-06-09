package main

import (
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

func tree() map[string]any {
	lv := &level{state: &protocol.StateInfo{
		Services: []*alphasfile.Service{{
			Toolchain: "go",
			Runtime: &alphasfile.RuntimeConfig{
				Name:      "prometheus",
				Vars:      map[string]any{"address": "127.0.0.1:9090", "port": float64(9090)},
				Arguments: map[string]any{"log.format": "json"},
				Command:   []string{"prometheus", "--config.file=x"},
				Dir:       "/repo",
			},
		}},
		Running: []protocol.ServiceStatus{{Name: "prometheus", PID: 4242, Readiness: "ready"}},
	}}
	return buildTree([]*level{lv})
}

func TestResolveExpr(t *testing.T) {
	root := tree()
	cases := []struct{ expr, want string }{
		{"service.go.prometheus.vars.address", "127.0.0.1:9090"},
		{"service.go.prometheus.vars.port", "9090"},
		{"service.go.prometheus.command.0", "prometheus"},
		{"service.go.prometheus.pid", "4242"},
		{"service.go.prometheus.ready", "true"},
		{"service.go.prometheus.toolchain", "go"},
		{"{{ .service.go.prometheus.vars.address }}", "127.0.0.1:9090"},
		{`{{ index .service.go.prometheus.arguments "log.format" }}`, "json"},
		{"{{ json .service.go.prometheus.command }}", `["prometheus","--config.file=x"]`},
	}
	for _, c := range cases {
		got, err := resolveExpr(root, c.expr)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.expr, got, c.want)
		}
	}
}

func TestResolveExpr_errors(t *testing.T) {
	root := tree()
	for _, expr := range []string{
		"service.go.nope.vars.x",            // unknown service
		"service.go.prometheus.vars.nope",   // unknown key
		"service.go.prometheus.vars.port.x", // descend into scalar
		"service.go.prometheus.command.9",   // slice out of range
		"{{ .service.go.prometheus.missing }}",
	} {
		if _, err := resolveExpr(root, expr); err == nil {
			t.Errorf("%s: expected error, got nil", expr)
		}
	}
}

func TestResolveExpr_errorListsAvailableKeys(t *testing.T) {
	_, err := resolveExpr(tree(), "service.go.prometheus.vars.nope")
	if err == nil || !strings.Contains(err.Error(), "address") {
		t.Fatalf("error should list available keys, got: %v", err)
	}
}
