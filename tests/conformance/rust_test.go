package conformance_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Rust conformance: one minimal-manifest regression per oneof choice
// in `service "rust" { ... }`. The source selector is a three-way
// oneof and the build/readiness selectors are two-way:
//
//	source:     src | git | cargo        (src covered; git/cargo deferred)
//	build:      default (cargo install --path) | explicit cmd
//	readiness:  http probe | none (stabilization-only)
//
// `git` (a github/gitlab/bitbucket clone) and `cargo` (a crates.io
// use-only install) both require network and a real remote — same
// reason the Go suite leaves its `git`/`package` variants as TODO.
// They are pinned as explicit Skips below so the oneof matrix is
// discoverable in code rather than implied by its absence.
//
// All Rust tests share golden/rust/echo (std-only, so `cargo install`
// is fast and offline) and reuse the Go suite's mustStart /
// mustDecodeEcho / readFile / echoResponse helpers — golden/rust/echo
// emits the identical JSON wire shape as golden/go/echo by design.
//
// The cargo bin target is named `echo` in golden/rust/echo/Cargo.toml,
// so every runtime cmd runs ${fs::bin()}/echo regardless of the HCL
// service name (cargo install names the artifact, not zordon).
//
// Cost note (same as go_test.go): the first run cold-bootstraps the
// pinned Rust toolchain via mise and cargo-builds the fixture
// (minutes). Subsequent runs reuse <repo>/.zordon and the shared
// CARGO_TARGET_DIR and finish in seconds.

const rustVersion = "1.83.0"

// TestRustService_srcDefaultBuild_httpReadiness is the canonical
// minimal shape: `src` primary + `exe`-derived default build (cargo
// install --path) + explicit runtime cmd + HTTP readiness probe. A
// regression in any of those four pieces breaks this.
func TestRustService_srcDefaultBuild_httpReadiness(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/echo")

	const port = 27700
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  rust {
    version = "%s"
  }
}

service "rust" "echo" {
  src {
    path = "./src/echo"
    exe = "."
  }

  vars = { port = %d }

  runtime {
    cmd = ["${fs::bin()}/echo", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, rustVersion, port))

	mustStart(t, p)
	mustGetRustEcho(t, port)
}

// TestRustService_srcExplicitBuildCmd swaps the `exe`-derived default
// build for an explicit `build { cmd = [...] }`. This exercises the
// svc.BuildCmd() branch in prepare() — distinct from defaultBuild's
// code path, so a break there can't hide behind the canonical test.
//
// Doubles as the happy-path pin for alpha's relookupPath: the cmd's
// first argv is the BARE name `cargo`. cargo is not on alpha's PATH
// (no rust on the CI runner, and even locally ~/.cargo/bin isn't in
// the sysenv whitelist), so exec.Command's initial LookPath fails
// and stores "exec: cargo: executable file not found" in cmd.Err.
// relookupPath must (1) point cmd.Path at the mise-pinned cargo it
// finds on the toolchain's PATH and (2) clear cmd.Err so Start()
// actually exec's the binary. Reaching `ready` proves both halves;
// TestRustService_explicitBuildCmdSurfacesCommandError is the
// negative twin that proves the cleared err didn't just mask the
// real one.
//
// The cmd reproduces what defaultBuild does (cargo install --path
// into <stateDir>/bin): cargo's --root X installs into X/bin, and
// ${fs::bin()}/.. + /bin collapses back to fs::bin(), so the runtime
// path ${fs::bin()}/echo finds the artifact with no extra fs:: fn.
func TestRustService_srcExplicitBuildCmd(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/echo")

	const port = 27701
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  rust {
    version = "%s"
  }
}

service "rust" "echo" {
  src { path = "./src/echo" }

  build {
    cmd = ["cargo", "install", "--path", ".", "--root", "${fs::bin()}/..", "--locked", "--force"]
  }

  vars = { port = %d }

  runtime {
    cmd = ["${fs::bin()}/echo", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, rustVersion, port))

	mustStart(t, p)
	mustGetRustEcho(t, port)
}

// TestRustService_noReadinessUsesStabilization asserts a Rust service
// with NO readiness block reaches "ready" via the stabilization timer
// ("stayed alive for N ms ⇒ ready"). Breakage in that fallback only
// affects probe-less daemons — the case least likely to be caught by
// hand, hence the dedicated regression. We still GET /echo after a
// beat to confirm the process really is up; the test's claim is that
// zordon didn't time out waiting for a probe that wasn't defined.
func TestRustService_noReadinessUsesStabilization(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/echo")

	const port = 27702
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  rust {
    version = "%s"
  }
}

service "rust" "echo" {
  src {
    path = "./src/echo"
    exe = "."
  }

  vars = { port = %d }

  runtime {
    cmd = ["${fs::bin()}/echo", "-addr", "127.0.0.1:${self.vars.port}"]
  }
}
`, rustVersion, port))

	mustStart(t, p)
	// Stabilization is ~1s by default; the listener binds immediately
	// but zordon doesn't probe, so wait before asserting it's up.
	time.Sleep(2 * time.Second)
	mustGetRustEcho(t, port)
}

// TestRustService_featuresReachBuild pins the Rust-specific `features`
// knob: it must reach `cargo install --features`. golden/rust/echo
// cfg-compiles a {"greeting":"on"} field only when its `greeting`
// feature is on, so observing that field in the echo JSON proves the
// feature flag traveled all the way from the Alphasfile into the
// compiled artifact (not just into the build argv).
func TestRustService_featuresReachBuild(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/echo")

	const port = 27703
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  rust {
    version = "%s"
  }
}

service "rust" "echo" {
  src {
    path = "./src/echo"
    exe = "."
  }
  features = ["greeting"]

  vars = { port = %d }

  runtime {
    cmd = ["${fs::bin()}/echo", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, rustVersion, port))

	mustStart(t, p)
	mustGetRustEcho(t, port)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var withFeature struct {
		Greeting string `json:"greeting"`
	}
	if err := json.Unmarshal(body, &withFeature); err != nil {
		t.Fatalf("decode echo: %v\nbody: %s", err, body)
	}
	if withFeature.Greeting != "on" {
		t.Errorf("greeting = %q; want \"on\" — `features = [\"greeting\"]` did not reach the build", withFeature.Greeting)
	}
}

// TestRustService_envBlockReachesRuntime asserts a service-level
// `env { }` block lands in the spawned Rust process's env. The echo
// fixture mirrors std::env::vars() back as JSON, so we read it and
// look for the key we set. (Language-agnostic in spirit, but the
// cargo-install build + spawn path is Rust-specific and worth its
// own pin alongside the Go twin.)
func TestRustService_envBlockReachesRuntime(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/echo")

	const port = 27704
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  rust {
    version = "%s"
  }
}

service "rust" "echo" {
  src {
    path = "./src/echo"
    exe = "."
  }

  vars = { port = %d }

  env = {
    GREETING = "hello-from-env-block"
  }

  runtime {
    cmd = ["${fs::bin()}/echo", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, rustVersion, port))

	mustStart(t, p)
	echo := mustDecodeEcho(t, port)
	if got := echo.Env["GREETING"]; got != "hello-from-env-block" {
		t.Errorf("GREETING in service env = %q; want %q", got, "hello-from-env-block")
	}
}

// TestRustService_provisionChainOrdering verifies a provision chain on
// a Rust service runs in dependency order: p1 has no `after`, p2 deps
// on p1.success. After bringup the test log must be exactly ["p1",
// "p2"]. Same pattern any user employs to test their own chains, so a
// break here is a break in user-facing behavior.
func TestRustService_provisionChainOrdering(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/echo")

	const port = 27705
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  rust {
    version = "%s"
  }
}

service "rust" "echo" {
  src {
    path = "./src/echo"
    exe = "."
  }

  vars = { port = %d }

  runtime {
    cmd = ["${fs::bin()}/echo", "-addr", "127.0.0.1:${self.vars.port}"]

    provision "p1" {
      cmd = test::log("p1")
    }

    provision "p2" {
      after = [self.runtime.provision.p1.success]
      cmd   = test::log("p2")
    }
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, rustVersion, port))

	mustStart(t, p)
	got := p.TestLog()
	want := []string{"p1", "p2"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("provision order = %v; want %v", got, want)
	}
}

// TestRustService_explicitBuildCmdSurfacesCommandError is the negative
// twin of TestRustService_srcExplicitBuildCmd and pins the second half
// of relookupPath's contract: after re-resolving a bare build-cmd name
// against the toolchain PATH, the cached LookPath error
// (exec.Command set cmd.Err the first time, against alpha's own PATH)
// MUST be cleared. If it isn't, exec.Cmd.Start() returns the stale
// "executable file not found" forever — the actual command never runs
// and the user debugs the wrong error. That's exactly how this whole
// bug class hid for a release: the happy-path test (srcExplicitBuildCmd)
// caught the missing relookupPath, but a follow-on regression on
// cmd.Err clearing would only be visible when the cmd itself fails.
//
// Construction: `cargo --zordon-bogus-flag` exec's a REAL cargo (so
// relookupPath worked) but cargo exits non-zero with its own argv
// parse error on stderr. That message must reach alpha's log; the
// stale "executable file not found" string must NOT. Runtime is a
// test::log marker the build gate must keep from running.
func TestRustService_explicitBuildCmdSurfacesCommandError(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/echo")

	const port = 27706
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  rust {
    version = "%s"
  }
}

service "rust" "echo" {
  src { path = "./src/echo" }

  build {
    cmd = ["cargo", "--zordon-bogus-flag"]
  }

  vars = { port = %d }

  runtime {
    cmd = ["sh", "-c", %s]
  }
}
`, rustVersion, port, "test::log(\"RUNTIME_STARTED_MUST_NOT_HAPPEN\")"))

	res := p.Zordon("start",
		"--timeout", "5m",
		"--alpha-log", p.AlphaLogPath(),
	).WithTimeout(6 * time.Minute).Run(t)
	if res.ExitCode == 0 {
		t.Fatalf("zordon start: exit 0 but expected failure (bogus cargo flag must failfast)")
	}

	for _, ln := range p.TestLog() {
		if ln == "RUNTIME_STARTED_MUST_NOT_HAPPEN" {
			t.Fatalf("runtime cmd executed despite build failing — build barrier broken")
		}
	}

	alphaLog, err := os.ReadFile(p.AlphaLogPath())
	if err != nil {
		t.Fatalf("read alpha log %s: %v", p.AlphaLogPath(), err)
	}
	logStr := string(alphaLog)
	if strings.Contains(logStr, "executable file not found") {
		t.Errorf("alpha log mentions \"executable file not found\" — relookupPath did not clear exec.Command's cached LookPath error;\nlog:\n%s", logStr)
	}
	if !strings.Contains(logStr, "--zordon-bogus-flag") {
		t.Errorf("alpha log lacks cargo's own error mentioning --zordon-bogus-flag — cargo's real stderr was not surfaced;\nlog:\n%s", logStr)
	}
}

// TestRustService_gitSource documents the `git` arm of the source
// oneof. NewPrimary's normalizeGit only accepts github.com /
// gitlab.com / bitbucket.org and rewrites to https://<repo>.git, so
// there is no offline/local-repo path — exercising it needs network
// and a published repo. Skipped (not deleted) so the oneof matrix
// stays explicit in code, matching go_test.go's "git ... TODO".
func TestRustService_gitSource(t *testing.T) {
	t.Skip("git source oneof requires a network clone from a supported host (github/gitlab/bitbucket); no offline path — see package doc")
}

// TestRustService_cargoUseOnly documents the `cargo` arm of the
// source oneof: a crates.io use-only install (cargo install <crate>,
// no checkout). Requires crates.io reachability and a crate that runs
// as a long-lived addressable server, so it's deferred for the same
// reason go_test.go defers `package`. Skipped to keep the matrix
// discoverable.
func TestRustService_cargoUseOnly(t *testing.T) {
	t.Skip("cargo (crates.io use-only) source oneof requires network + a long-lived server crate; deferred like go_test.go's package variant")
}

// --- rust-specific helpers (package-local) ---------------------
//
// mustStart / mustDecodeEcho / readFile / echoResponse are defined in
// go_test.go (same package conformance_test) and reused as-is.

// mustGetRustEcho is the Rust analogue of mustGetEcho: it confirms the
// service is reachable AND that the binary was compiled by the
// mise-pinned toolchain. golden/rust/echo's build.rs captures
// `rustc --version` into ECHO_RUSTC, which surfaces as
// runtime_version like "rustc 1.83.0 (...)".
func mustGetRustEcho(t *testing.T, port int) {
	t.Helper()
	echo := mustDecodeEcho(t, port)
	if !strings.Contains(echo.RuntimeVersion, rustVersion) {
		t.Errorf("runtime_version = %q; want it to contain pinned rust %s", echo.RuntimeVersion, rustVersion)
	}
}
