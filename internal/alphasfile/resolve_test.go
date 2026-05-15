package alphasfile

import (
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// testInv is a fixed, deterministic invocation so assertions don't depend
// on $TMPDIR / cwd. Resolution must be pure: no clone, no spawn, no fs.
func testInv() *invocation.Invocation {
	return &invocation.Invocation{
		Dir:      "/proj",
		Worktree: "main",
		StateDir: "/proj/.zordon/worktrees/main",
		Hash:     "abcd1234ef567890",
		TmpDir:   "/tmp/zordon-abcd1234ef567890",
	}
}

func compile(t *testing.T, src string, parent *ParentContext) *Alphasfile {
	t.Helper()
	af, err := Compile("test.hcl", []byte(src), testInv(), parent)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return af
}

func svcByName(af *Alphasfile, name string) *Service {
	for _, s := range af.All() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func TestResolveInvocationDerivedValues(t *testing.T) {
	src := `
service "go" "api" {
  git = "github.com/acme/api"
  vars = { hash = pathhash(), tmp = tmpdir() }
  file "env" {
    path = "${tmpdir()}/.env"
    body = "DIR=${self.dir}\nHASH=${pathhash()}\n"
  }
  command = ["./api", "-data", "${self.dir}/data"]
}
`
	af := compile(t, src, nil)
	api := svcByName(af, "api")
	if api == nil {
		t.Fatal("service api not resolved")
	}
	if got := api.Runtime.Vars["hash"]; got != "abcd1234ef567890" {
		t.Errorf("pathhash() = %v, want invocation hash", got)
	}
	if got := api.Runtime.Vars["tmp"]; got != "/tmp/zordon-abcd1234ef567890" {
		t.Errorf("tmpdir() = %v", got)
	}
	wantDir := "/proj/.zordon/worktrees/main/src/api"
	if api.Runtime.Dir != wantDir {
		t.Errorf("self.dir resolved = %q, want %q", api.Runtime.Dir, wantDir)
	}
	if len(api.Runtime.Files) != 1 {
		t.Fatalf("want 1 file, got %d", len(api.Runtime.Files))
	}
	f := api.Runtime.Files[0]
	if f.Path != "/tmp/zordon-abcd1234ef567890/.env" {
		t.Errorf("file path = %q", f.Path)
	}
	if !strings.Contains(f.Body, "DIR="+wantDir) || !strings.Contains(f.Body, "HASH=abcd1234ef567890") {
		t.Errorf("file body interpolation wrong:\n%s", f.Body)
	}
	wantCmd := []string{"./api", "-data", wantDir + "/data"}
	if strings.Join(api.Runtime.Command, " ") != strings.Join(wantCmd, " ") {
		t.Errorf("command = %v, want %v", api.Runtime.Command, wantCmd)
	}
}

func TestResolveCrossServiceRefAndDir(t *testing.T) {
	src := `
service "go" "db" {
  git  = "github.com/acme/db"
  vars = { port = 5432 }
}
service "go" "api" {
  git  = "github.com/acme/api"
  vars = { db_at = "${service.go.db.dir}@${service.go.db.vars.port}" }
}
`
	af := compile(t, src, nil)
	api := svcByName(af, "api")
	want := "/proj/.zordon/worktrees/main/src/db@5432"
	if got := api.Runtime.Vars["db_at"]; got != want {
		t.Errorf("cross-service ref = %q, want %q", got, want)
	}
}

func TestResolveFederationParentFlatNamespace(t *testing.T) {
	parent := NewParentContext([]*Service{{
		Toolchain: "go",
		Runtime:   &RuntimeConfig{Name: "caddy", Dir: "/other/.zordon/worktrees/main/src/caddy", Vars: map[string]any{"http": int64(8080)}},
		Package:   &Package{Toolchain: "go", Git: "github.com/caddyserver/caddy"},
	}})
	src := `
service "go" "app" {
  git  = "github.com/acme/app"
  vars = { upstream = "127.0.0.1:${service.go.caddy.vars.http}", caddydir = service.go.caddy.dir }
}
`
	af := compile(t, src, parent)
	app := svcByName(af, "app")
	if got := app.Runtime.Vars["upstream"]; got != "127.0.0.1:8080" {
		t.Errorf("parent vars ref = %q", got)
	}
	if got := app.Runtime.Vars["caddydir"]; got != "/other/.zordon/worktrees/main/src/caddy" {
		t.Errorf("parent dir ref = %q", got)
	}
}

func TestResolveGitDirExclusive(t *testing.T) {
	_, err := Compile("t", []byte(`
service "go" "x" {
  git = "github.com/a/b"
  dir = "/home/me/x"
}
`), testInv(), nil)
	if err == nil || !strings.Contains(err.Error(), "both git and dir") {
		t.Fatalf("want git/dir exclusivity error, got %v", err)
	}
}

func TestResolveNameCollision(t *testing.T) {
	_, err := Compile("t", []byte(`
service "go" "dup" { git = "github.com/a/b" }
service "go" "dup" { git = "github.com/a/c" }
`), testInv(), nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate service") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestResolveNonWorktreeableHasNoDir(t *testing.T) {
	// crate / bare binary: no git, no dir ⇒ not worktree-able, dir empty.
	af := compile(t, `service "rust" "tansu" { crate = "tansu" }`, nil)
	s := svcByName(af, "tansu")
	if s.Worktreeable() {
		t.Error("crate service should not be worktree-able")
	}
	if s.Runtime.Dir != "" {
		t.Errorf("non-worktree service dir = %q, want empty", s.Runtime.Dir)
	}
}

func TestResolveSudoStepsResolved(t *testing.T) {
	src := `
service "go" "dns" {
  git  = "github.com/acme/dns"
  vars = { port = 5300 }
  sudo "resolver" {
    check  = "grep -q ${self.vars.port} /etc/resolver/test"
    apply  = "echo ${self.vars.port} > /etc/resolver/test"
    verify = "test -f /etc/resolver/test"
  }
}
`
	af := compile(t, src, nil)
	dns := svcByName(af, "dns")
	if len(dns.Runtime.Sudo) != 1 {
		t.Fatalf("want 1 sudo step, got %d", len(dns.Runtime.Sudo))
	}
	st := dns.Runtime.Sudo[0]
	if st.Name != "resolver" || !strings.Contains(st.Check, "5300") || !strings.Contains(st.Apply, "5300") {
		t.Errorf("sudo step not resolved: %+v", st)
	}
}
