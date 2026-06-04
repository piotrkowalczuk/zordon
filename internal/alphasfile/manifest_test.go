package alphasfile

import "testing"

// The staged pipeline's payoff: a dependency cycle is a PLANNING error, caught
// by Plan (topo-sort) before Compute runs any effectful HCL function. Parse
// (NewManifestState) still succeeds — a cycle is structural, not syntactic.
func TestManifestState_PlanCatchesCycleBeforeCompute(t *testing.T) {
	src := `
service "go" "a" {
  git { url = "github.com/x/a" }
  vars = { v = service.go.b.vars.v }
}
service "go" "b" {
  git { url = "github.com/x/b" }
  vars = { v = service.go.a.vars.v }
}
`
	m, err := NewManifestState("cycle.hcl", []byte(src), testInv())
	if err != nil {
		t.Fatalf("parse must succeed (a cycle is a planning error, not syntax): %v", err)
	}
	if _, err := m.Plan(nil, testCfgHash, TestConfig{}); err == nil {
		t.Fatal("Plan must fail on a vars↔vars cycle, before any Compute")
	}
}

// A clean manifest stages all the way through: parse → Plan → Compute, with
// cfg::hash() (threaded via Plan) surfacing in the computed result.
func TestManifestState_PlanComputeStages(t *testing.T) {
	src := `
service "go" "api" {
  git { url = "github.com/x/api" }
  vars = { cfg = cfg::hash() }
}
`
	m, err := NewManifestState("ok.hcl", []byte(src), testInv())
	if err != nil {
		t.Fatalf("NewManifestState: %v", err)
	}
	p, err := m.Plan(nil, testCfgHash, TestConfig{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	af, err := p.Compute()
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if af.CfgHash != testCfgHash {
		t.Errorf("Alphasfile.CfgHash = %q, want %q", af.CfgHash, testCfgHash)
	}
	api := svcByName(af, "api")
	if api == nil || api.Runtime.Vars["cfg"] != testCfgHash {
		t.Errorf("cfg::hash() through Plan/Compute = %v, want %q", api.Runtime.Vars["cfg"], testCfgHash)
	}
}

// fs::src() is the checkout ROOT (≈ src.path); fs::exe() is the exe-anchored
// working dir (≈ src.path + src.exe), which equals self.dir. With src{exe} set
// they DIFFER — that's why both exist. (Without exe they coincide.)
func TestResolve_FsSrcRootVsFsExe(t *testing.T) {
	src := `
service "go" "api" {
  git { url = "github.com/acme/api" }
  src { exe = "./cmd/api" }
  vars = {
    root = fs::src()
    exe  = fs::exe()
    dir  = self.dir
  }
}
`
	af := compile(t, src, nil)
	api := svcByName(af, "api")
	if api == nil {
		t.Fatal("service api not resolved")
	}
	const wantRoot = "/proj/.zordon/worktrees/main/src/api"
	const wantExe = wantRoot + "/cmd/api"
	if got := api.Runtime.Vars["root"]; got != wantRoot {
		t.Errorf("fs::src() = %v, want checkout root %q", got, wantRoot)
	}
	if got := api.Runtime.Vars["exe"]; got != wantExe {
		t.Errorf("fs::exe() = %v, want exe-anchored %q", got, wantExe)
	}
	if got := api.Runtime.Vars["dir"]; got != wantExe {
		t.Errorf("self.dir = %v, want exe-anchored %q (== fs::exe())", got, wantExe)
	}
	if api.Runtime.Vars["root"] == api.Runtime.Vars["exe"] {
		t.Error("fs::src() and fs::exe() must differ when src.exe is set")
	}
	// The wire-stable split alpha consumes: Dir is the exe-anchored
	// build/run cwd, Checkout is the checkout root that `git worktree add`
	// targets. Conflating them broke worktree services with exe != "."
	// (see TestWorktree_Go_exeOffset).
	if got := api.Runtime.Checkout; got != wantRoot {
		t.Errorf("Runtime.Checkout = %q, want checkout root %q (the worktree-add target)", got, wantRoot)
	}
	if got := api.Runtime.Dir; got != wantExe {
		t.Errorf("Runtime.Dir = %q, want exe-anchored %q (the build/run cwd)", got, wantExe)
	}
}

// fs::src() is a SERVICE-scope function. At file scope (top-level dotenv) there
// is no checkout, so it must error — NOT leak a prior service's stale checkout,
// which is exactly the footgun the scope split fixes.
func TestCompute_FsSrcErrorsAtFileScope(t *testing.T) {
	src := `
service "go" "a" { git { url = "github.com/x/a" } }
dotenv = "${fs::src()}/.env"
`
	if _, err := Compile("fs.hcl", []byte(src), testInv(), nil, testCfgHash, TestConfig{}); err == nil {
		t.Fatal("fs::src() in top-level dotenv (file scope) must error, not return a stale checkout")
	}
}
