package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

func provByName(s *Service, name string) *ProvisionStep {
	if s == nil || s.Runtime == nil {
		return nil
	}
	for _, p := range s.Runtime.Provision {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// Resolution oracle for examples/invoke: `after = never` makes kafka's
// create-topic latent (declared, not auto-run), and a consumer whose
// provision `cmd` is a bare reference to it resolves to a CmdRef with
// no inline cmd. The example gates each invoker on `self.runtime.ready`
// explicitly. Pure Compile — no spawn.
func TestExampleInvokeResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/invoke/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.Invocation{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		Worktree: invocation.MainWorktree,
		StateDir: "/repo/examples/invoke/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/invoke/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	tpl := provByName(svcByName(af, "kafka"), "create-topic")
	if tpl == nil {
		t.Fatal("kafka.create-topic not resolved")
	}
	if !tpl.Latent {
		t.Errorf("create-topic.Latent = false; want true (after = never)")
	}
	if len(tpl.After) != 0 {
		t.Errorf("latent provision After = %v; want empty", tpl.After)
	}
	if !strings.Contains(tpl.Cmd, "topics.txt") || strings.Contains(tpl.Cmd, "${") {
		t.Errorf("create-topic.Cmd not resolved against kafka self: %q", tpl.Cmd)
	}

	for _, svc := range []string{"app", "billing"} {
		p := provByName(svcByName(af, svc), "topic")
		if p == nil {
			t.Fatalf("%s.topic not resolved", svc)
		}
		if p.CmdRef != "service.go.kafka.runtime.provision.create-topic" {
			t.Errorf("%s.topic.CmdRef = %q; want kafka's create-topic id", svc, p.CmdRef)
		}
		if p.Cmd != "" {
			t.Errorf("%s.topic.Cmd = %q; want empty (cmd is a ref)", svc, p.Cmd)
		}
		if p.Latent {
			t.Errorf("%s.topic.Latent = true; want false", svc)
		}
		want := "service.go." + svc + ".runtime@ready"
		if len(p.After) != 1 || p.After[0] != want {
			t.Errorf("%s.topic.After = %v; want [%s] (explicit self.runtime.ready)", svc, p.After, want)
		}
	}
}
