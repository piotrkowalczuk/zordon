package main

import (
	"io"
	"net"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

func TestResolveProvisionArgs(t *testing.T) {
	decls := []*alphasfile.ProvisionArg{
		{Name: "key", Type: "string", Required: true},
		{Name: "n", Type: "number", Default: int64(3)},
		{Name: "opt", Type: "string"},
	}

	t.Run("supplied + default + empty optional", func(t *testing.T) {
		got, err := resolveProvisionArgs(decls, map[string]any{"key": "abc"})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		for k, want := range map[string]string{"key": "abc", "n": "3", "opt": ""} {
			if got[k] != want {
				t.Errorf("%q = %q; want %q", k, got[k], want)
			}
		}
	})

	t.Run("missing required", func(t *testing.T) {
		if _, err := resolveProvisionArgs(decls, nil); err == nil || !strings.Contains(err.Error(), "missing required argument \"key\"") {
			t.Fatalf("want missing-required error, got %v", err)
		}
	})

	t.Run("unknown argument", func(t *testing.T) {
		if _, err := resolveProvisionArgs(decls, map[string]any{"key": "a", "bogus": 1}); err == nil || !strings.Contains(err.Error(), "unknown argument \"bogus\"") {
			t.Fatalf("want unknown-argument error, got %v", err)
		}
	})

	t.Run("json number coerced to int form", func(t *testing.T) {
		// MCP/JSON decodes numbers as float64.
		got, err := resolveProvisionArgs(decls, map[string]any{"key": "a", "n": float64(7)})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if got["n"] != "7" {
			t.Errorf("n = %q; want \"7\"", got["n"])
		}
	})

	t.Run("type mismatch rejected", func(t *testing.T) {
		mismatches := map[string]map[string]any{
			"string for number": {"key": "a", "n": "not-a-number"},
			"number for string": {"key": 5},
			"string for bool":   {"key": "a", "flag": "true"},
		}
		bools := []*alphasfile.ProvisionArg{
			{Name: "key", Type: "string", Required: true},
			{Name: "n", Type: "number", Default: int64(3)},
			{Name: "flag", Type: "bool"},
		}
		for name, in := range mismatches {
			t.Run(name, func(t *testing.T) {
				if _, err := resolveProvisionArgs(bools, in); err == nil || !strings.Contains(err.Error(), "must be a") {
					t.Fatalf("want type error, got %v", err)
				}
			})
		}
	})
}

func TestCoerceArg(t *testing.T) {
	cases := map[string]struct {
		typ     string
		in      any
		want    string
		wantErr bool
	}{
		"number float integral": {"number", float64(3), "3", false},
		"number float decimal":  {"number", float64(3.5), "3.5", false},
		"number int64 default":  {"number", int64(8), "8", false},
		"number from string":    {"number", "x", "", true},
		"bool true":             {"bool", true, "true", false},
		"bool from string":      {"bool", "true", "", true},
		"string ok":             {"string", "hi", "hi", false},
		"string from number":    {"string", float64(1), "", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := coerceArg(&alphasfile.ProvisionArg{Name: "a", Type: c.typ}, c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("coerceArg(%s,%v) = %q,%v; want %q,nil", c.typ, c.in, got, err, c.want)
			}
		})
	}
}

func TestSubstituteArgs(t *testing.T) {
	snippet := "echo " + alphasfile.ArgSentinel("key") + " > " + alphasfile.ArgSentinel("path")
	got := substituteArgs(snippet, map[string]string{"key": "hello", "path": "/tmp/x"})
	if want := "echo hello > /tmp/x"; got != want {
		t.Errorf("substituteArgs = %q; want %q", got, want)
	}
	if got := substituteArgs("", map[string]string{"k": "v"}); got != "" {
		t.Errorf("empty snippet = %q; want empty", got)
	}
}

// TestHandleInvoke_validation covers the branches that reject before any
// provision actually runs. The success/failure-while-alpha-survives paths
// need a live alpha and are exercised end-to-end in examples/mcp.
func TestHandleInvoke_validation(t *testing.T) {
	cases := map[string]struct {
		setup   func(s *alphaState)
		req     *protocol.Request
		wantErr string
	}{
		"missing provision id": {
			setup:   func(*alphaState) {},
			req:     &protocol.Request{Op: protocol.OpInvoke, Invoke: &protocol.InvokeArgs{}},
			wantErr: "missing provision id",
		},
		"nil invoke args": {
			setup:   func(*alphaState) {},
			req:     &protocol.Request{Op: protocol.OpInvoke},
			wantErr: "missing provision id",
		},
		"unknown provision": {
			setup:   func(*alphaState) {},
			req:     &protocol.Request{Op: protocol.OpInvoke, Invoke: &protocol.InvokeArgs{Provision: "service.go.app.runtime.provision.seed"}},
			wantErr: "unknown provision",
		},
		"service not running": {
			setup: func(s *alphaState) {
				s.addProvision(newProvisionCtx("service.go.app", &alphasfile.ProvisionStep{Name: "seed", Latent: true}))
			},
			req:     &protocol.Request{Op: protocol.OpInvoke, Invoke: &protocol.InvokeArgs{Provision: "service.go.app.runtime.provision.seed"}},
			wantErr: "is not running",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			t.Parallel()
			s := &alphaState{}
			c.setup(s)

			client, server := net.Pipe()
			defer client.Close()
			go func() {
				handleInvoke(c.req, s, bringupConfig{}, protocol.NewEncoder(server), zlog.New(io.Discard, true))
				_ = server.Close()
			}()

			var ev protocol.Event
			if err := protocol.NewDecoder(client).Read(&ev); err != nil {
				t.Fatalf("read event: %v", err)
			}
			if ev.Kind != protocol.EventError {
				t.Fatalf("kind = %q; want %q", ev.Kind, protocol.EventError)
			}
			if !strings.Contains(ev.Error, c.wantErr) {
				t.Errorf("error = %q; want substring %q", ev.Error, c.wantErr)
			}
		})
	}
}
