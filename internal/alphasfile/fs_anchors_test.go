package alphasfile

import (
	"strings"
	"testing"
)

// fs::etc() / fs::var() are the per-service persistent anchors: generated
// config that must survive (etc) and variable runtime state (var), both under
// <StateDir>/<sub>/<svc>. Unlike fs::tmp() they are never reaped.
func TestResolveFsEtcVar(t *testing.T) {
	src := `
service "go" "api" {
  git { url = "github.com/acme/api" }
  vars = { conf = "${fs::etc()}/app.conf", data = "${fs::var()}/db" }
  file "conf" {
    path = "${fs::etc()}/app.conf"
    body = "listen 8080\n"
  }
}
`
	af := compile(t, src, nil)
	api := svcByName(af, "api")
	if api == nil {
		t.Fatal("service api not resolved")
	}
	if got, want := api.Runtime.Vars["conf"], "/proj/workspaces/main/etc/api/app.conf"; got != want {
		t.Errorf("fs::etc() = %q, want %q", got, want)
	}
	if got, want := api.Runtime.Vars["data"], "/proj/workspaces/main/var/api/db"; got != want {
		t.Errorf("fs::var() = %q, want %q", got, want)
	}
	if len(api.Runtime.Files) != 1 {
		t.Fatalf("want 1 file, got %d", len(api.Runtime.Files))
	}
	if got, want := api.Runtime.Files[0].Path, "/proj/workspaces/main/etc/api/app.conf"; got != want {
		t.Errorf("file path anchored at fs::etc() = %q, want %q", got, want)
	}
	// The per-service anchor dirs are persisted on the runtime so alpha can
	// pre-create them at bringup without re-deriving the layout.
	if got, want := api.Runtime.EtcDir, "/proj/workspaces/main/etc/api"; got != want {
		t.Errorf("Runtime.EtcDir = %q, want %q", got, want)
	}
	if got, want := api.Runtime.VarDir, "/proj/workspaces/main/var/api"; got != want {
		t.Errorf("Runtime.VarDir = %q, want %q", got, want)
	}
}

// fs::service::etc / fs::service::var name a same-invocation peer's persistent
// dir, mirroring fs::service::bin. self resolves to the caller's own dir.
func TestResolveFsServiceEtcVar(t *testing.T) {
	src := `
service "go" "db" {
  git { url = "github.com/acme/db" }
}
service "go" "api" {
  git { url = "github.com/acme/api" }
  vars = {
    db_etc  = fs::service::etc(service.go.db)
    db_var  = fs::service::var(service.go.db)
    own_etc = fs::service::etc(self)
  }
}
`
	af := compile(t, src, nil)
	api := svcByName(af, "api")
	if got, want := api.Runtime.Vars["db_etc"], "/proj/workspaces/main/etc/db"; got != want {
		t.Errorf("fs::service::etc(db) = %q, want %q", got, want)
	}
	if got, want := api.Runtime.Vars["db_var"], "/proj/workspaces/main/var/db"; got != want {
		t.Errorf("fs::service::var(db) = %q, want %q", got, want)
	}
	if got, want := api.Runtime.Vars["own_etc"], "/proj/workspaces/main/etc/api"; got != want {
		t.Errorf("fs::service::etc(self) = %q, want %q", got, want)
	}
}

// fs::etc()/fs::var() are service-scoped: at file scope (top-level env) they
// have no service dir and must error clearly, exactly like fs::src().
func TestResolveFsEtcFileScopeErrors(t *testing.T) {
	_, err := Compile("t", []byte(`
env = { X = "${fs::etc()}" }
service "go" "api" {
  git { url = "github.com/acme/api" }
}
`), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), "no invocation/service context") {
		t.Fatalf("want file-scope fs::etc() error, got %v", err)
	}
}

// A non-service argument to fs::service::* is a manifest error, not a silent
// bad path.
func TestResolveFsServiceEtcRejectsNonService(t *testing.T) {
	_, err := Compile("t", []byte(`
service "go" "api" {
  git { url = "github.com/acme/api" }
  vars = { bad = fs::service::etc("nope") }
}
`), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), "expected a service reference") {
		t.Fatalf("want service-reference error, got %v", err)
	}
}
