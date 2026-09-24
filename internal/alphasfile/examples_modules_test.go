package alphasfile

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// examples/modules: two modules with colliding service names plus an
// entrypoint-level gateway wired through module.* refs. Pure Compile.
func TestExampleModulesResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/modules/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "abc0000011112222", TmpDir: "/tmp/zordon-abc0000011112222",
		Workspace: invocation.MainWorkspace, StateDir: "/repo/examples/modules/workspaces/main",
	}
	af, err := Compile("/repo/examples/modules/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	want := map[string]string{
		"gateway":      "/repo/examples/modules/src/gateway",
		"payments/db":  "/repo/examples/modules/src/payments_db",
		"payments/api": "/repo/examples/modules/src/payments_api",
		"auth/db":      "/repo/examples/modules/src/auth_db",
		"auth/api":     "/repo/examples/modules/src/auth_api",
	}
	if len(af.Services) != len(want) {
		t.Fatalf("want %d services, got %v", len(want), serviceNames(af))
	}
	for name, dir := range want {
		s := svcByName(af, name)
		if s == nil {
			t.Fatalf("%s missing; have %v", name, serviceNames(af))
		}
		if s.Runtime.Dir != dir {
			t.Errorf("%s dir = %q, want %q", name, s.Runtime.Dir, dir)
		}
		if !s.Package.InPlace {
			t.Errorf("%s must be in-place in main", name)
		}
	}

	payAPI, authAPI := svcByName(af, "payments/api"), svcByName(af, "auth/api")
	gw := svcByName(af, "gateway")
	upstream := fmt.Sprintf("payments=127.0.0.1:%v,auth=127.0.0.1:%v", payAPI.Runtime.Vars["port"], authAPI.Runtime.Vars["port"])
	if cmd := strings.Join(gw.Runtime.Command, " "); !strings.Contains(cmd, upstream) {
		t.Errorf("gateway cmd %q lacks %q", cmd, upstream)
	}
	if got := gw.Runtime.After; len(got) != 2 || got[0] != "module.payments.service.go.api.runtime@ready" || got[1] != "module.auth.service.go.api.runtime@ready" {
		t.Errorf("gateway after = %v", got)
	}

	// Each api's bare `service.go.db` resolved to its own module's db.
	for _, m := range []string{"payments", "auth"} {
		api, db := svcByName(af, m+"/api"), svcByName(af, m+"/db")
		wantUp := fmt.Sprintf("db=127.0.0.1:%v", db.Runtime.Vars["port"])
		if cmd := strings.Join(api.Runtime.Command, " "); !strings.Contains(cmd, wantUp) {
			t.Errorf("%s/api cmd %q lacks %q", m, cmd, wantUp)
		}
		if got := api.Runtime.After; len(got) != 1 || got[0] != "module."+m+".service.go.db.runtime@ready" {
			t.Errorf("%s/api after = %v", m, got)
		}
		if !strings.Contains(strings.Join(db.Runtime.Command, " "), "-name "+m+"/db") {
			t.Errorf("%s/db cmd = %v", m, db.Runtime.Command)
		}
	}
	if svcByName(af, "payments/db").Runtime.Vars["port"] == svcByName(af, "auth/db").Runtime.Vars["port"] {
		t.Error("the two db services must get distinct ports")
	}
}
