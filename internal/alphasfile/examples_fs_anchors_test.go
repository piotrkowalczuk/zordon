package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for examples/fs_anchors: fs::etc()/fs::var() anchor a
// service's durable config/state under <state>/etc|var/<svc> (never fs::tmp),
// and fs::service::etc(peer) resolves a peer's etc dir — the backend drops a
// fragment straight into gateway's etc. Pure Compile — no spawn.
func TestExampleFsAnchorsResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/fs_anchors/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		Workspace: invocation.MainWorkspace,
		StateDir:  "/repo/examples/fs_anchors/workspaces/main",
	}
	af, err := Compile("/repo/examples/fs_anchors/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	gw := svcByName(af, "gateway")
	if gw == nil || gw.Runtime == nil {
		t.Fatal("gateway not resolved")
	}
	conf := fileByName(gw, "conf")
	if conf == nil {
		t.Fatal("gateway file{conf} not resolved")
	}
	if want := "/repo/examples/fs_anchors/workspaces/main/etc/gateway/gateway.conf"; conf.Path != want {
		t.Errorf("fs::etc() file path = %q, want %q", conf.Path, want)
	}
	if strings.Contains(conf.Path, "/tmp/") {
		t.Errorf("durable config must not live under tmp: %q", conf.Path)
	}
	cmd := strings.Join(gw.Runtime.Command, " ")
	if want := "/repo/examples/fs_anchors/workspaces/main/var/gateway/state"; !strings.Contains(cmd, want) {
		t.Errorf("fs::var() not in cmd %q, want substring %q", cmd, want)
	}

	// Cross-service: backend writes into gateway's etc dir, not its own.
	be := svcByName(af, "backend")
	if be == nil {
		t.Fatal("backend not resolved")
	}
	frag := fileByName(be, "fragment")
	if frag == nil {
		t.Fatal("backend file{fragment} not resolved")
	}
	if want := "/repo/examples/fs_anchors/workspaces/main/etc/gateway/backend.conf"; frag.Path != want {
		t.Errorf("fs::service::etc(gateway) file path = %q, want %q", frag.Path, want)
	}
}

func fileByName(s *Service, name string) *File {
	if s.Runtime == nil {
		return nil
	}
	for _, f := range s.Runtime.Files {
		if f.Name == name {
			return f
		}
	}
	return nil
}
