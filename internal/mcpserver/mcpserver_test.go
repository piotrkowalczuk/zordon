package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/zenv"
)

func svc(toolchain, name string, steps ...*alphasfile.ProvisionStep) *alphasfile.Service {
	return &alphasfile.Service{
		Toolchain: toolchain,
		Runtime:   &alphasfile.RuntimeConfig{Name: name, Provision: steps},
	}
}

func TestProvisions_flatten(t *testing.T) {
	services := []*alphasfile.Service{
		svc("go", "kafka", &alphasfile.ProvisionStep{Name: "create-topic", Latent: true, Cmd: `echo "$TOPIC" >> /tmp/topics`}),
		svc("go", "app",
			&alphasfile.ProvisionStep{Name: "seed", Cmd: "seed.sh", Env: zenv.EnvironmentVariables{"KEY": "v", "REGION": "eu"}},
			&alphasfile.ProvisionStep{Name: "notify", Detached: true, Cmd: "notify.sh"},
		),
		{Toolchain: "go", Runtime: nil}, // runtime-less service contributes nothing
	}

	got := Provisions(services)
	if len(got) != 3 {
		t.Fatalf("len = %d; want 3 (got %+v)", len(got), got)
	}

	kafka := got[0]
	if kafka.ID != "service.go.kafka.runtime.provision.create-topic" {
		t.Errorf("kafka.ID = %q", kafka.ID)
	}
	if !kafka.Latent {
		t.Error("kafka.Latent = false; want true")
	}
	if want := "provision__go_kafka__create-topic"; kafka.ToolName() != want {
		t.Errorf("kafka.ToolName() = %q; want %q", kafka.ToolName(), want)
	}

	seed := got[1]
	if want := []string{"KEY", "REGION"}; strings.Join(seed.EnvKeys, ",") != strings.Join(want, ",") {
		t.Errorf("seed.EnvKeys = %v; want sorted %v", seed.EnvKeys, want)
	}
	desc := seed.Description()
	for _, want := range []string{`"seed"`, "go.app", "seed.sh", "KEY, REGION", "never shuts alpha down"} {
		if !strings.Contains(desc, want) {
			t.Errorf("seed.Description() = %q; missing %q", desc, want)
		}
	}

	if !got[2].Detached {
		t.Error("notify.Detached = false; want true")
	}
}

func TestProvision_Description_authorDocLeads(t *testing.T) {
	services := []*alphasfile.Service{
		svc("go", "app", &alphasfile.ProvisionStep{Name: "seed", Description: "Seed the database with fixtures", Cmd: "seed.sh"}),
	}
	got := Provisions(services)
	if len(got) != 1 {
		t.Fatalf("len = %d; want 1", len(got))
	}
	if got[0].Doc != "Seed the database with fixtures" {
		t.Errorf("Doc = %q; want the authored text", got[0].Doc)
	}
	desc := got[0].Description()
	if !strings.HasPrefix(desc, "Seed the database with fixtures.") {
		t.Errorf("Description() = %q; want the author's doc first", desc)
	}
	if !strings.Contains(desc, "never shuts alpha down") {
		t.Errorf("Description() = %q; missing synthesized invariant", desc)
	}

	// No description: falls back to the synthesized form.
	plain := Provisions([]*alphasfile.Service{svc("go", "app", &alphasfile.ProvisionStep{Name: "x", Cmd: "true"})})
	if d := plain[0].Description(); !strings.HasPrefix(d, "Run provision") {
		t.Errorf("Description() without doc = %q; want it to start with 'Run provision'", d)
	}
}

func TestProvision_InputSchema(t *testing.T) {
	p := Provision{Args: []*alphasfile.ProvisionArg{
		{Name: "key", Type: "string", Required: true, Description: "the key"},
		{Name: "count", Type: "number", Default: int64(3)},
		{Name: "force", Type: "bool"},
	}}
	s := p.InputSchema()

	if s["type"] != "object" {
		t.Errorf("type = %v; want object", s["type"])
	}
	props := s["properties"].(map[string]any)
	for name, wantType := range map[string]string{"key": "string", "count": "number", "force": "boolean"} {
		prop, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("missing property %q", name)
		}
		if prop["type"] != wantType {
			t.Errorf("%q.type = %v; want %v", name, prop["type"], wantType)
		}
	}
	if props["key"].(map[string]any)["description"] != "the key" {
		t.Errorf("key.description missing")
	}
	if props["count"].(map[string]any)["default"] != int64(3) {
		t.Errorf("count.default = %v; want 3", props["count"].(map[string]any)["default"])
	}
	// generic env escape hatch always present
	env, ok := props["env"].(map[string]any)
	if !ok || env["type"] != "object" {
		t.Errorf("env property missing or wrong: %v", props["env"])
	}
	req, _ := s["required"].([]string)
	if len(req) != 1 || req[0] != "key" {
		t.Errorf("required = %v; want [key]", s["required"])
	}

	// Round-trips to valid JSON.
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("schema not JSON-marshalable: %v", err)
	}
}

func TestSanitizeToolName(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"dots to underscore":       {"service.go.app.runtime.provision.seed", "service_go_app_runtime_provision_seed"},
		"keeps hyphen underscore":  {"provision__go_app__seed-data", "provision__go_app__seed-data"},
		"spaces and slashes":       {"a b/c", "a_b_c"},
		"collapses invalid runs":   {"a@@@b", "a_b"},
		"empty becomes underscore": {"", "_"},
		"clamps to 64":             {strings.Repeat("x", 80), strings.Repeat("x", 64)},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got := SanitizeToolName(c.in)
			if got != c.want {
				t.Errorf("SanitizeToolName(%q) = %q; want %q", c.in, got, c.want)
			}
			if len(got) > 64 {
				t.Errorf("len = %d; want <= 64", len(got))
			}
		})
	}
}

func TestUnique_disambiguates(t *testing.T) {
	seen := map[string]struct{}{}
	a := Unique(seen, "provision__go_app__seed")
	b := Unique(seen, "provision__go_app__seed")
	c := Unique(seen, "provision__go_app__seed")
	if a != "provision__go_app__seed" {
		t.Errorf("first = %q; want unchanged base", a)
	}
	if b == a || c == a || b == c {
		t.Errorf("collisions not disambiguated: %q %q %q", a, b, c)
	}
}
