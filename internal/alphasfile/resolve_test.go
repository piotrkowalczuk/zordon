package alphasfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// testCfgHash is the fixed manifest identity threaded into Compile in tests
// (cfg::hash() returns it). It lives apart from testInv because CfgHash is no
// longer a directory fact carried by Invocation.
const testCfgHash = "00000000cfg00000"

// testInv is a fixed, deterministic invocation so assertions don't depend
// on $TMPDIR / cwd. Resolution must be pure: no clone, no spawn, no fs.
func testInv() *invocation.InvocationState {
	return &invocation.InvocationState{
		Dir:       "/proj",
		Workspace: "main",
		StateDir:  "/proj/workspaces/main",
		FsHash:    "abcd1234ef567890",
		TmpDir:    "/tmp/zordon-abcd1234ef567890",
	}
}

func compile(t *testing.T, src string, parent *ParentContext) *Alphasfile {
	t.Helper()
	af, err := Compile("test.hcl", []byte(src), testInv(), parent, testCfgHash, TestConfig{})
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
  git { url = "github.com/acme/api" }
  vars = { fs = fs::hash(), cfg = cfg::hash(), tmp = fs::tmp() }
  file "env" {
    path = "${fs::tmp()}/.env"
    body = "DIR=${self.dir}\nFS=${fs::hash()}\nCFG=${cfg::hash()}\n"
  }
  runtime {
    cmd = ["./api", "-data", "${self.dir}/data"]
  }
}
`
	af := compile(t, src, nil)
	api := svcByName(af, "api")
	if api == nil {
		t.Fatal("service api not resolved")
	}
	if got := api.Runtime.Vars["fs"]; got != "abcd1234ef567890" {
		t.Errorf("fs::hash() = %v, want invocation FsHash", got)
	}
	if got := api.Runtime.Vars["cfg"]; got != "00000000cfg00000" {
		t.Errorf("cfg::hash() = %v, want invocation CfgHash", got)
	}
	if got := api.Runtime.Vars["tmp"]; got != "/tmp/zordon-abcd1234ef567890" {
		t.Errorf("fs::tmp() = %v", got)
	}
	wantDir := "/proj/workspaces/main/src/api"
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
	if !strings.Contains(f.Body, "DIR="+wantDir) ||
		!strings.Contains(f.Body, "FS=abcd1234ef567890") ||
		!strings.Contains(f.Body, "CFG=00000000cfg00000") {
		t.Errorf("file body interpolation wrong:\n%s", f.Body)
	}
	wantCmd := []string{"./api", "-data", wantDir + "/data"}
	if strings.Join(api.Runtime.Command, " ") != strings.Join(wantCmd, " ") {
		t.Errorf("command = %v, want %v", api.Runtime.Command, wantCmd)
	}
}

// fs::state() returns the per-workspace state root
// (`<repo>/workspaces/<wt>`) — a stable location services can
// drop persistent caches into (Bootsnap, Vite, sccache) that survive
// across `zordon start` but are scoped per workspace. fs::tmp is
// invocation-scoped (volatile), fs::bin is for built binaries; state
// is the right home for "regenerated on demand, but expensive enough
// to keep across runs".
func TestResolveFsState(t *testing.T) {
	src := `
service "go" "api" {
  git { url = "github.com/acme/api" }
  vars = { cache_dir = "${fs::state()}/cache/bootsnap" }
}
`
	af := compile(t, src, nil)
	api := svcByName(af, "api")
	want := "/proj/workspaces/main/cache/bootsnap"
	if got := api.Runtime.Vars["cache_dir"]; got != want {
		t.Errorf("fs::state() interpolation = %q, want %q", got, want)
	}
}

func TestResolveCrossServiceRefAndDir(t *testing.T) {
	src := `
service "go" "db" {
  git { url = "github.com/acme/db" }
  vars = { port = 5432 }
}
service "go" "api" {
  git { url = "github.com/acme/api" }
  vars = { db_at = "${service.go.db.dir}@${service.go.db.vars.port}" }
}
`
	af := compile(t, src, nil)
	api := svcByName(af, "api")
	want := "/proj/workspaces/main/src/db@5432"
	if got := api.Runtime.Vars["db_at"]; got != want {
		t.Errorf("cross-service ref = %q, want %q", got, want)
	}
}

func TestResolveFederationParentFlatNamespace(t *testing.T) {
	parent := NewParentContext([]*Service{{
		Toolchain: "go",
		Runtime:   &RuntimeConfig{Name: "caddy", Dir: "/other/workspaces/main/src/caddy", Vars: map[string]any{"http": int64(8080)}},
		Package:   &Package{Toolchain: "go", Git: "github.com/caddyserver/caddy"},
	}})
	src := `
service "go" "app" {
  git { url = "github.com/acme/app" }
  vars = { upstream = "127.0.0.1:${service.go.caddy.vars.http}", caddydir = service.go.caddy.dir }
}
`
	af := compile(t, src, parent)
	app := svcByName(af, "app")
	if got := app.Runtime.Vars["upstream"]; got != "127.0.0.1:8080" {
		t.Errorf("parent vars ref = %q", got)
	}
	if got := app.Runtime.Vars["caddydir"]; got != "/other/workspaces/main/src/caddy" {
		t.Errorf("parent dir ref = %q", got)
	}
}

func TestResolveUseOnlyExcludesSrc(t *testing.T) {
	_, err := Compile("t", []byte(`
service "rust" "x" {
  src { path = "../.." }
  crate { name = "tansu" }
}
`), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), "cannot coexist") {
		t.Fatalf("want use-only/src exclusivity error, got %v", err)
	}
}

// `git { }` + `src { exe }` is the canonical remote-with-subdir form:
// git carries the URL (and ref); src carries only the build subdir
// within the cloned workspace (no path).
func TestResolveGitWithSrcExe(t *testing.T) {
	af := compile(t, `
service "go" "api" {
  git {
    url = "github.com/a/api"
    tag = "v1.0.0"
  }
  src { exe = "./cmd/api" }
}
`, nil)
	s := svcByName(af, "api")
	if s == nil || !s.Workspaceable() || s.UseOnly() {
		t.Fatalf("git+src{exe} must be a workspace service: %+v", s.Package)
	}
	if s.Package.Git != "github.com/a/api" || s.Package.Tag != "v1.0.0" || s.Package.Exe != "./cmd/api" {
		t.Errorf("git+src{exe} not carried: %+v", s.Package)
	}
}

// src{path} + git{} together must be rejected — they're alternative
// source primaries (local-only vs. remote-cloned).
func TestResolveSrcPathExcludesGit(t *testing.T) {
	_, err := Compile("t", []byte(`
service "go" "x" {
  git { url = "github.com/a/x" }
  src { path = "../checkouts/x" }
}
`), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want src{path}/git mutual-exclusion error, got %v", err)
	}
}

func TestResolveNameCollision(t *testing.T) {
	_, err := Compile("t", []byte(`
service "go" "dup" {
  git { url = "github.com/a/b" }
}
service "go" "dup" {
  git { url = "github.com/a/c" }
}
`), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), "duplicate service") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestResolveUseOnlyNoWorkspace(t *testing.T) {
	// rust use-only block: installed, not workspace-able, no checkout dir.
	af := compile(t, `
service "rust" "tansu" {
  crate { name = "tansu" }
}
`, nil)
	s := svcByName(af, "tansu")
	if !s.UseOnly() || s.Workspaceable() || !s.Buildable() {
		t.Errorf("use-only flags wrong: useOnly=%v wt=%v build=%v", s.UseOnly(), s.Workspaceable(), s.Buildable())
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
  crate {
    name    = "tansu"
    version = "0.6.0"
  }
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
	// rust-only `crate {}` block on a go service is a typed mistake.
	_, err := Compile("t", []byte(`
service "go" "x" {
  crate { name = "tansu" }
}
`), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), "rust-only") {
		t.Fatalf("want toolchain/field mismatch error, got %v", err)
	}
}

// Relative `src` paths resolve against the Alphasfile's OWN directory
// (the file that contains them), not against CWD where zordon happened
// to be invoked from. Without this, running `zordon start` from a
// subdir would compute different paths than running it from the
// project root — same Alphasfile, different resolved checkouts. Workspaces
// have the same need (they adopt the project-root Alphasfile and run
// from a state subdir).
func TestResolveSrc_resolvesAgainstAlphasfileDir(t *testing.T) {
	// Compile with an absolute Alphasfile path so the test pins exactly
	// where afDir lands; relative `src = "../tools"` then expands to
	// /tmp/proj-root/tools (one level up from /tmp/proj-root/inner/).
	src := `
service "go" "tooling" {
  src { path = "../tools" }
}
`
	afPath := "/tmp/test-resolve-src/proj/Alphasfile"
	af, err := Compile(afPath, []byte(src), testInv(), nil, testCfgHash, TestConfig{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got := svcByName(af, "tooling").Package.Src
	want := "/tmp/test-resolve-src/tools"
	if got != want {
		t.Errorf("src resolved to %q; want %q (relative to Alphasfile dir, not CWD)", got, want)
	}
}

// src{path} is an interpolated expression: host-env helpers like
// os::env let the checkout location be derived at eval time. An
// absolute env value stays absolute through resolveSrcDir.
func TestResolveSrcPathUsesFunctions(t *testing.T) {
	t.Setenv("ZORDON_SRC_ROOT", "/tmp/monorepo")
	src := `
service "go" "api" {
  src { path = "${os::env("ZORDON_SRC_ROOT")}/services/api" }
}
`
	af := compile(t, src, nil)
	got := svcByName(af, "api").Package.Src
	want := "/tmp/monorepo/services/api"
	if got != want {
		t.Errorf("src.path with os::env resolved to %q; want %q", got, want)
	}
}

// The parse-only path (ParseServices, used by `zordon workspace`)
// resolves src{path} too, with the host-level function set available.
func TestParseServicesSrcPathUsesFunctions(t *testing.T) {
	t.Setenv("ZORDON_SRC_ROOT", "/tmp/monorepo")
	dir := t.TempDir()
	afPath := filepath.Join(dir, "Alphasfile")
	body := `
service "go" "api" {
  src { path = "${os::env("ZORDON_SRC_ROOT")}/services/api" }
}
`
	if err := os.WriteFile(afPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	metas, err := ParseServices(afPath)
	if err != nil {
		t.Fatalf("ParseServices: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("want 1 service, got %d", len(metas))
	}
	got := metas[0].Package.Src
	want := "/tmp/monorepo/services/api"
	if got != want {
		t.Errorf("ParseServices src.path with os::env resolved to %q; want %q", got, want)
	}
}

// Invocation/identity namespaces (fs::, cfg::, ...) need a live run and
// are absent in the parse-only context: referencing one in src{path} is
// a clear error, not a silent empty checkout.
func TestParseServicesSrcPathRejectsInvocationFunc(t *testing.T) {
	dir := t.TempDir()
	afPath := filepath.Join(dir, "Alphasfile")
	body := `
service "go" "api" {
  src { path = "${fs::tmp()}/api" }
}
`
	if err := os.WriteFile(afPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseServices(afPath); err == nil {
		t.Fatal("want error for invocation-only function in parse-only src.path, got nil")
	}
}

// Two services that cross-reference each other's vars from inside env
// must NOT trigger a cycle: A.env→B.vars and B.env→A.vars are
// independent edges between four nodes (A.vars, A.env, B.vars,
// B.env), not a cycle between A and B. Before the per-producer DAG,
// the service-level graph collapsed all of a service's expressions
// into one node and falsely flagged this as cyclic.
func TestResolveCrossServiceEnvVarsNoFalseCycle(t *testing.T) {
	src := `
service "go" "a" {
  git { url = "github.com/acme/a" }
  vars = { port = 5000 }
  env  = { B_PORT = "${service.go.b.vars.port}" }
}
service "go" "b" {
  git { url = "github.com/acme/b" }
  vars = { port = 6000 }
  env  = { A_PORT = "${service.go.a.vars.port}" }
}
`
	af := compile(t, src, nil)
	a, b := svcByName(af, "a"), svcByName(af, "b")
	if a == nil || b == nil {
		t.Fatal("a/b not resolved")
	}
	if got := a.Runtime.Env["B_PORT"]; got != "6000" {
		t.Errorf("a.env.B_PORT = %q, want 6000 (interpolated from b.vars.port)", got)
	}
	if got := b.Runtime.Env["A_PORT"]; got != "5000" {
		t.Errorf("b.env.A_PORT = %q, want 5000 (interpolated from a.vars.port)", got)
	}
}

// A genuine cycle on the same producer kind (A.vars depends on B.vars
// AND vice-versa) must still be reported — the per-producer DAG
// shouldn't silently accept impossible-to-resolve definitions.
func TestResolveCrossServiceVarsCycleStillCaught(t *testing.T) {
	src := `
service "go" "a" {
  git { url = "github.com/acme/a" }
  vars = { x = service.go.b.vars.x }
}
service "go" "b" {
  git { url = "github.com/acme/b" }
  vars = { x = service.go.a.vars.x }
}
`
	_, err := Compile("t", []byte(src), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error for vars↔vars mutual ref, got %v", err)
	}
}

func TestResolveSudoStepsResolved(t *testing.T) {
	src := `
service "go" "dns" {
  git { url = "github.com/acme/dns" }
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

// Resolve provisions: check/cmd/verify get interpolated against self
// and cross-service vars; env is captured as a string map; after refs
// resolve to canonical "<entityID>@<state>" strings ready for alpha
// to look up at bringup. Cross-service barrier traversals (e.g.
// service.go.db.ready) also drive the dep graph — without that edge
// db.vars wouldn't be resolved before api's provision evaluates.
func TestResolveProvisionsResolved(t *testing.T) {
	src := `
service "go" "db" {
  git { url = "github.com/acme/db" }
  vars = { port = 5432, password = "secret" }
}
service "go" "api" {
  git { url = "github.com/acme/api" }
  vars = { port = 8080 }
  runtime {
    provision "create-tables" {
      check  = "psql -p ${service.go.db.vars.port} -tc 'SELECT 1'"
      cmd    = "psql -p ${service.go.db.vars.port} -f schema.sql"
      verify = "psql -p ${service.go.db.vars.port} -tc 'SELECT 1 FROM users LIMIT 0'"
      env    = { PGPASSWORD = service.go.db.vars.password }
      after  = [service.go.db.runtime.ready]
    }
    provision "seed" {
      cmd      = "psql -f seed.sql"
      after    = [self.runtime.provision.create-tables.success]
      detached = true
    }
  }
}
`
	af := compile(t, src, nil)
	api := svcByName(af, "api")
	if api == nil || len(api.Runtime.Provision) != 2 {
		t.Fatalf("want 2 provisions on api, got %+v", api)
	}
	create := api.Runtime.Provision[0]
	if create.Name != "create-tables" {
		t.Errorf("first provision name = %q, want create-tables", create.Name)
	}
	if !strings.Contains(create.Check, "psql -p 5432") {
		t.Errorf("check not interpolated with db port: %q", create.Check)
	}
	if !strings.Contains(create.Cmd, "psql -p 5432 -f schema.sql") {
		t.Errorf("cmd not interpolated: %q", create.Cmd)
	}
	if create.Env["PGPASSWORD"] != "secret" {
		t.Errorf("env PGPASSWORD = %q, want secret", create.Env["PGPASSWORD"])
	}
	if len(create.After) != 1 || create.After[0] != "service.go.db.runtime@ready" {
		t.Errorf("after = %v, want [service.go.db.runtime@ready]", create.After)
	}
	if create.Detached {
		t.Errorf("create-tables.Detached = true, want false (default)")
	}

	seed := api.Runtime.Provision[1]
	if !seed.Detached {
		t.Errorf("seed.Detached = false, want true (explicit)")
	}
	wantSeedAfter := "service.go.api.runtime.provision.create-tables@success"
	if len(seed.After) != 1 || seed.After[0] != wantSeedAfter {
		t.Errorf("seed.After = %v, want [%s]", seed.After, wantSeedAfter)
	}
}

func TestResolveReadinessExec(t *testing.T) {
	src := `
service "go" "db" {
  git { url = "github.com/acme/db" }
  vars = { port = 5432 }
  runtime { cmd = ["./db"] }
  readiness {
    exec {
      command = ["pg_isready", "-h", "127.0.0.1", "-p", "${self.vars.port}"]
      env     = { PGUSER = "zordon" }
    }
    period            = "200ms"
    failure_threshold = 30
  }
}
`
	af := compile(t, src, nil)
	db := svcByName(af, "db")
	if db == nil {
		t.Fatal("service db not resolved")
	}
	p := db.Runtime.Readiness
	if p == nil || p.Exec == nil {
		t.Fatal("readiness exec action not resolved")
	}
	want := []string{"pg_isready", "-h", "127.0.0.1", "-p", "5432"}
	if len(p.Exec.Command) != len(want) {
		t.Fatalf("command = %v, want %v", p.Exec.Command, want)
	}
	for i := range want {
		if p.Exec.Command[i] != want[i] {
			t.Fatalf("command = %v, want %v", p.Exec.Command, want)
		}
	}
	if p.Exec.Env["PGUSER"] != "zordon" {
		t.Errorf("exec.env PGUSER = %q, want zordon", p.Exec.Env["PGUSER"])
	}
	if p.HTTP != nil {
		t.Errorf("exec probe should not carry an http action")
	}
	if p.FailureThreshold != 30 {
		t.Errorf("failure_threshold = %d, want 30", p.FailureThreshold)
	}
}

func TestResolveReadinessActionErrors(t *testing.T) {
	cases := map[string]struct {
		readiness string
		wantErr   string
	}{
		"both http and exec": {
			readiness: `readiness {
    http { port = 8080 }
    exec { command = ["true"] }
  }`,
			wantErr: "not both",
		},
		"neither http nor exec": {
			readiness: `readiness { period = "1s" }`,
			wantErr:   "requires an http or exec action",
		},
		"empty exec command": {
			readiness: `readiness {
    exec {
      command = []
    }
  }`,
			wantErr: "must not be empty",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			src := `
service "go" "db" {
  git { url = "github.com/acme/db" }
  runtime { cmd = ["./db"] }
  ` + c.readiness + `
}
`
			_, err := Compile("test.hcl", []byte(src), testInv(), nil, testCfgHash, TestConfig{})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), c.wantErr)
			}
		})
	}
}
