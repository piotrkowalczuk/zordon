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
  cmd = ["./api", "-data", "${self.dir}/data"]
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

func TestResolveUseOnlyExcludesSrc(t *testing.T) {
	_, err := Compile("t", []byte(`
service "rust" "x" {
  src   = "../.."
  cargo = "tansu"
}
`), testInv(), nil)
	if err == nil || !strings.Contains(err.Error(), "use-only") {
		t.Fatalf("want use-only/src exclusivity error, got %v", err)
	}
}

func TestResolveGitSeedsSrc(t *testing.T) {
	// git + src is allowed: git is the origin that seeds src; worktree
	// from src. Both present must not error.
	af := compile(t, `
service "go" "api" {
  git = "github.com/a/api"
  src = "../checkouts/api"
}
`, nil)
	s := svcByName(af, "api")
	if s == nil || !s.Worktreeable() || s.UseOnly() {
		t.Fatalf("git+src must be a worktree service: %+v", s.Package)
	}
	if s.Package.Git != "github.com/a/api" || s.Package.Src == "" {
		t.Errorf("git+src not carried: %+v", s.Package)
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

func TestResolveUseOnlyNoWorktree(t *testing.T) {
	// rust use-only block: installed, not worktree-able, no checkout dir.
	af := compile(t, `
service "rust" "tansu" {
  cargo = "tansu"
}
`, nil)
	s := svcByName(af, "tansu")
	if !s.UseOnly() || s.Worktreeable() || !s.Buildable() {
		t.Errorf("use-only flags wrong: useOnly=%v wt=%v build=%v", s.UseOnly(), s.Worktreeable(), s.Buildable())
	}
	if s.Package.Install != "tansu" {
		t.Errorf("install = %q, want tansu", s.Package.Install)
	}
	if s.Runtime.Dir != "" {
		t.Errorf("use-only dir = %q, want empty", s.Runtime.Dir)
	}
}

func TestResolveGoUseOnly(t *testing.T) {
	af := compile(t, `
service "go" "dlv" {
  package = "github.com/go-delve/delve/cmd/dlv@latest"
}
`, nil)
	s := svcByName(af, "dlv")
	if !s.UseOnly() || s.Package.Install != "github.com/go-delve/delve/cmd/dlv@latest" {
		t.Errorf("go use-only wrong: useOnly=%v install=%q", s.UseOnly(), s.Package.Install)
	}
}

func TestResolveRustUseOnlyFeaturesVersion(t *testing.T) {
	af := compile(t, `
service "rust" "tansu" {
  cargo    = "tansu"
  version  = "0.6.0"
  features = ["a", "b"]
}
`, nil)
	s := svcByName(af, "tansu")
	if s.Package.Install != "tansu" || s.Package.Version != "0.6.0" {
		t.Errorf("cargo/version wrong: %+v", s.Package)
	}
	if len(s.Package.Features) != 2 || s.Package.Features[0] != "a" || s.Package.Features[1] != "b" {
		t.Errorf("features = %v", s.Package.Features)
	}
}

func TestResolveToolchainFieldMismatch(t *testing.T) {
	// rust use-only field on a go service is a typed mistake.
	_, err := Compile("t", []byte(`
service "go" "x" {
  cargo = "tansu"
}
`), testInv(), nil)
	if err == nil || !strings.Contains(err.Error(), "rust use-only") {
		t.Fatalf("want toolchain/field mismatch error, got %v", err)
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
