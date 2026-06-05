package protocol

import (
	"bytes"
	"testing"
)

func TestRequest_RoundTrip(t *testing.T) {
	cases := map[string]struct {
		in   Request
		want Request
	}{
		"invoke with env": {
			in: Request{Op: OpInvoke, Invoke: &InvokeArgs{
				Provision: "service.go.kafka.runtime.provision.create-topic",
				Env:       map[string]string{"TOPIC": "app-events"},
			}},
			want: Request{Op: OpInvoke, Invoke: &InvokeArgs{
				Provision: "service.go.kafka.runtime.provision.create-topic",
				Env:       map[string]string{"TOPIC": "app-events"},
			}},
		},
		"invoke with args": {
			in: Request{Op: OpInvoke, Invoke: &InvokeArgs{
				Provision: "service.go.app.runtime.provision.seed",
				Args:      map[string]any{"key": "abc"},
			}},
			want: Request{Op: OpInvoke, Invoke: &InvokeArgs{
				Provision: "service.go.app.runtime.provision.seed",
				Args:      map[string]any{"key": "abc"},
			}},
		},
		"invoke without env": {
			in:   Request{Op: OpInvoke, Invoke: &InvokeArgs{Provision: "service.go.app.runtime.provision.seed"}},
			want: Request{Op: OpInvoke, Invoke: &InvokeArgs{Provision: "service.go.app.runtime.provision.seed"}},
		},
		"shutdown has no invoke": {
			in:   Request{Op: OpShutdown},
			want: Request{Op: OpShutdown},
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			var buf bytes.Buffer
			if err := NewEncoder(&buf).Write(&c.in); err != nil {
				t.Fatalf("encode: %v", err)
			}
			var got Request
			if err := NewDecoder(&buf).Read(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Op != c.want.Op {
				t.Errorf("Op = %q; want %q", got.Op, c.want.Op)
			}
			switch {
			case c.want.Invoke == nil && got.Invoke != nil:
				t.Fatalf("Invoke = %+v; want nil", got.Invoke)
			case c.want.Invoke != nil && got.Invoke == nil:
				t.Fatalf("Invoke = nil; want %+v", c.want.Invoke)
			case c.want.Invoke != nil:
				if got.Invoke.Provision != c.want.Invoke.Provision {
					t.Errorf("Invoke.Provision = %q; want %q", got.Invoke.Provision, c.want.Invoke.Provision)
				}
				if len(got.Invoke.Env) != len(c.want.Invoke.Env) {
					t.Fatalf("Invoke.Env = %v; want %v", got.Invoke.Env, c.want.Invoke.Env)
				}
				for k, v := range c.want.Invoke.Env {
					if got.Invoke.Env[k] != v {
						t.Errorf("Invoke.Env[%q] = %q; want %q", k, got.Invoke.Env[k], v)
					}
				}
				if len(got.Invoke.Args) != len(c.want.Invoke.Args) {
					t.Fatalf("Invoke.Args = %v; want %v", got.Invoke.Args, c.want.Invoke.Args)
				}
				for k, v := range c.want.Invoke.Args {
					if got.Invoke.Args[k] != v {
						t.Errorf("Invoke.Args[%q] = %v; want %v", k, got.Invoke.Args[k], v)
					}
				}
			}
		})
	}
}

// TestRequest_BackwardCompat asserts a pre-OpInvoke wire message (no "invoke"
// field) still decodes — alpha and zordon may be built from different commits.
func TestRequest_BackwardCompat(t *testing.T) {
	const legacy = `{"op":"configure","configure":{"alphasfile_path":"/x/Alphasfile","alphasfile":null}}` + "\n"
	var got Request
	if err := NewDecoder(bytes.NewBufferString(legacy)).Read(&got); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if got.Op != OpConfigure {
		t.Errorf("Op = %q; want %q", got.Op, OpConfigure)
	}
	if got.Invoke != nil {
		t.Errorf("Invoke = %+v; want nil", got.Invoke)
	}
}
