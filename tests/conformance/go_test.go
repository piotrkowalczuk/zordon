// Package conformance contains behavioral tests of zordon's externally
// observable behavior — driven through the zordon CLI using the
// zordontest harness, asserting on HTTP responses, log contents,
// registry state, and exit codes.
//
// These tests are NOT zordontest's own tests (that library's
// self-tests live in internal/zordontest/project_test.go). They use
// zordontest as a tool to exercise zordon end-to-end and pin specific
// behaviors of the supervisor + Alphasfile evaluator + toolchain
// bringup pipeline.
//
// File-per-language convention: tests/conformance/<lang>_test.go
// holds the minimal-manifest variants and toolchain-specific
// scenarios for that language. Cross-language scenarios (federation,
// concurrent bringup, registry collision) get their own files.
//
// Cost: first run cold-caches mise (~5-7 min cargo install) and any
// pinned toolchain (~1-2 min mise install). Subsequent runs reuse
// <repo>/.zordon and finish in seconds. The per-test timeout is
// generous; CI should set --timeout >= 20m for a guaranteed-clean run.
package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Each oneof choice in `service "go" { ... }` gets a minimal-manifest
// regression below:
//
//	primary:    src | git | package         (3 variants — git, package TODO)
//	build:      default (from exe) | explicit cmd
//	readiness:  http probe | none (stabilization-only)
//
// Tests share golden/go/echo as the source fixture so build-cache
// hits across them on the shared <repo>/.zordon cache.

// TestGoService_srcDefaultBuild_httpReadiness is the canonical minimal
// shape: src primary + exe-derived default build + explicit runtime
// cmd + HTTP readiness probe. Catches regressions where any of those
// four pieces breaks the happy path.
func TestGoService_srcDefaultBuild_httpReadiness(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
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
`)

	mustStart(t, p)
	port := p.Get(t, "service.go.svc1.vars.port").Int()
	mustGetEcho(t, port)
}

// TestGoService_srcExplicitBuildCmd swaps `exe`-derived defaultBuild
// for an explicit `build { cmd = [...] }` block. Exercises the
// `svc.BuildCmd()` branch in prepare() — a distinct code path from
// defaultBuild and worth its own regression so a break there doesn't
// hide behind the default-path's test passing.
func TestGoService_srcExplicitBuildCmd(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src { path = "./src/svc1" }

  build {
    cmd = ["go", "build", "-o", "${fs::bin()}/svc1", "."]
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
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
`)

	mustStart(t, p)
	port := p.Get(t, "service.go.svc1.vars.port").Int()
	mustGetEcho(t, port)
}

// TestGoService_noReadinessUsesStabilization asserts that a service
// with NO readiness block reaches "ready" via the stabilization
// timer (the "stayed alive for N ms = ready" fallback). Without
// this regression, breakage in the stabilization path would only
// affect daemons without probes — exactly the case least likely to
// be caught manually.
//
// We still hit the echo endpoint after a brief delay to confirm
// the service IS up; the test's claim is that zordon didn't time
// out waiting for a probe that wasn't defined.
func TestGoService_noReadinessUsesStabilization(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
  }
}
`)

	mustStart(t, p)
	// Stabilization is ~1s by default; service binds quickly but
	// zordon doesn't probe, so wait a beat before asserting.
	time.Sleep(2 * time.Second)
	port := p.Get(t, "service.go.svc1.vars.port").Int()
	mustGetEcho(t, port)
}

// TestGoService_envBlockReachesRuntime asserts that a service-level
// `env { }` block actually lands in the spawned process's env. The
// golden echo service mirrors os.Environ() in its JSON response, so
// we read it back and look for the key we set.
func TestGoService_envBlockReachesRuntime(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  env = {
    GREETING = "hello-from-env-block"
  }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
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
`)

	mustStart(t, p)
	port := p.Get(t, "service.go.svc1.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	if got := echo.Env["GREETING"]; got != "hello-from-env-block" {
		t.Errorf("GREETING in service env = %q; want %q", got, "hello-from-env-block")
	}
}

// TestGoService_runtimeEnvOverridesBaseEnv pins the precedence:
// service-level `env { K=A }` is the BASE, phase `runtime { env { K=B } }`
// overlays on top. The service must see B. Without this regression a
// silent swap of merge order would let stale base values leak through.
func TestGoService_runtimeEnvOverridesBaseEnv(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  env = {
    LAYER = "base"
  }

  runtime {
    env = {
      LAYER = "runtime"
    }
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
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
`)

	mustStart(t, p)
	port := p.Get(t, "service.go.svc1.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	if got := echo.Env["LAYER"]; got != "runtime" {
		t.Errorf("LAYER = %q; want runtime to override base", got)
	}
}

// TestGoService_varsInterpolationInCmd covers cross-cutting: `vars`
// declared at the service level + ${self.vars.X} interpolated into
// the runtime cmd argv. Both the cmd template (alphasfile resolver)
// and arg passing (alpha spawn) have to work for the service to
// receive the value.
//
// The fixture's echo includes argv in its JSON output, so we can
// confirm the resolved value reached the binary as a literal flag.
func TestGoService_varsInterpolationInCmd(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = {
    port = net::pickport()
    tag  = "alpha-bravo"
  }

  runtime {
    cmd = [
      "${fs::bin()}/svc1",
      "-addr", "127.0.0.1:${self.vars.port}",
      "-tag", "${self.vars.tag}",
    ]
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
`)

	mustStart(t, p)
	port := p.Get(t, "service.go.svc1.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	// echo.Argv is os.Args of the spawned process: [<bin>, -addr, ..., -tag, alpha-bravo]
	joined := strings.Join(echo.Argv, " ")
	if !strings.Contains(joined, "-tag alpha-bravo") {
		t.Errorf("argv missing interpolated -tag alpha-bravo: %v", echo.Argv)
	}
}

// TestGoService_fileBlockMaterialized covers the `file { }` block:
// alpha writes the body to disk at the resolved path BEFORE the
// service starts. We assert by reading the file from the test side
// — golden/go/echo doesn't read files, so we exercise the on-disk
// effect directly (the most reliable assertion: the file is there
// and contains exactly what the Alphasfile said it should).
//
// Using vars + interpolation in the body proves the file body goes
// through the same evaluator as cmd/argv.
func TestGoService_fileBlockMaterialized(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	// Pin the generated file to a stable relative path so we can
	// read it back from the test.
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = {
    port = net::pickport()
    who  = "world"
  }

  file "config" {
    path = "${fs::tmp()}/generated.conf"
    body = "hello, ${self.vars.who} on port ${self.vars.port}\n"
  }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
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
`)

	mustStart(t, p)

	// fs::tmp() resolves to $TMPDIR/zordon-<fsHash>; the test doesn't
	// know fsHash directly, but the file path was interpolated into
	// the Alphasfile and `zordon get` can resolve it.
	// `file` is keyed by the HCL block's name label — `file "config"
	// { ... }` → service.go.svc1.file.config.*.
	confPath := p.Get(t, "service.go.svc1.file.config.path").String()
	data, err := readFile(t, confPath)
	if err != nil {
		t.Fatalf("read generated file %s: %v", confPath, err)
	}
	port := p.Get(t, "service.go.svc1.vars.port").Int()
	want := fmt.Sprintf("hello, world on port %d\n", port)
	if data != want {
		t.Errorf("generated file body = %q; want %q", data, want)
	}
}

// TestGoService_sysenvWhitelistStripsHostVar pins the closed-world
// guarantee: an env var present in the harness's process env but
// NOT in Alphasfile.sysenv must NOT reach the spawned service. Same
// var IS reachable when sysenv allows it through. Two assertions
// in one test because they're the two halves of the same contract
// — splitting wouldn't add signal.
func TestGoService_sysenvWhitelistStripsHostVar(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")
	t.Setenv("HOST_VAR_ALLOWED", "yes-allowed")
	t.Setenv("HOST_VAR_DENIED", "should-be-stripped")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR", "HOST_VAR_ALLOWED"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
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
`)

	mustStart(t, p)
	port := p.Get(t, "service.go.svc1.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	if got := echo.Env["HOST_VAR_ALLOWED"]; got != "yes-allowed" {
		t.Errorf("whitelisted HOST_VAR_ALLOWED = %q; want yes-allowed", got)
	}
	if _, present := echo.Env["HOST_VAR_DENIED"]; present {
		t.Errorf("HOST_VAR_DENIED leaked through despite not being in sysenv")
	}
}

// TestGoService_provisionChainOrdering uses the test:: HCL namespace
// to verify that a provision chain runs in the expected order. p1
// has no `after`, p2 depends on p1.success. After bringup, the
// test log should contain "p1" then "p2" — never in reverse,
// never with "p2" alone.
//
// This is the same pattern any user could employ to test their own
// provision chains: drop test::log into provision cmds and assert
// on the resulting log lines.
func TestGoService_provisionChainOrdering(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]

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
`)

	mustStart(t, p)
	got := p.TestLog()
	want := []string{"p1", "p2"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("provision order = %v; want %v", got, want)
	}
}

// TestGoService_provisionFailureSkipsDependent: a provision that
// fails (via test::fail) must short-circuit its dependent — the
// dependent's cmd must NOT run, so its test::log marker NEVER
// appears in the log. Failfast also tears down the whole bringup;
// we tolerate the non-zero exit and just inspect the log.
//
// This is the negative-path twin of TestGoService_provisionChainOrdering:
// chain semantics are pinned both for success propagation AND
// failure propagation.
func TestGoService_provisionFailureSkipsDependent(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]

    provision "broken" {
      cmd = test::fail("intentional failure for test")
    }

    provision "should-not-run" {
      after = [self.runtime.provision.broken.success]
      cmd   = test::log("UNREACHABLE")
    }
  }
}
`)

	// Failfast will kill the bringup; we expect non-zero exit.
	p.Start(t, zordontest.StartTimeout(5*time.Minute)).Failed()

	for _, ln := range p.TestLog() {
		if ln == "UNREACHABLE" {
			t.Errorf("dependent provision ran despite its dep failing — chain breaker broken")
		}
	}
}

// TestGoService_modcacheSharedByDefaultIsolatedOnOverride is the
// behavioral proof that zordon's "trust mise + Go defaults" stance
// for the build/mod cache is sound:
//
//   - Service "fetcher": its provision runs `go mod download <pkg>`.
//     The module lands in the default GOMODCACHE
//     ($HOME/go/pkg/mod/cache/download/<pkg>/@v/<ver>.zip).
//   - Service "shared-check": its provision (after fetcher.success)
//     asserts the very same file exists in default GOMODCACHE.
//     If this fails, default cache is NOT shared and zordon would
//     need to manage it.
//   - Service "isolated-check": its provision overrides GOMODCACHE
//     via per-step env, then asserts the file does NOT exist in
//     the isolated path. If this fails, our isolation mechanism
//     (via runtime/provision env) is broken.
//
// All three services share one Alphasfile so the chain runs in
// one zordon start: each provision's exit code IS the assertion.
// No HTTP probing of runtime needed — provisions either succeed
// (zordon emits success barrier) or fail (failfast triggers).
//
// Module choice: rsc.io/quote v1.5.2 — tiny (one .go file), public,
// commonly used in Go docs as a hello-world module; first download
// is fast even on cold CI runners.
func TestGoService_modcacheSharedByDefaultIsolatedOnOverride(t *testing.T) {
	const (
		modulePath    = "rsc.io/quote"
		moduleVersion = "v1.5.2"
		// Where Go's default GOMODCACHE stores the download zip.
		// $HOME/go/pkg/mod/ is Go's default GOPATH/pkg/mod.
		sharedZipRel = "go/pkg/mod/cache/download/rsc.io/quote/@v/v1.5.2.zip"
	)
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")
	p.CopyTree("golden/go/echo", "src/svc2")
	p.CopyTree("golden/go/echo", "src/svc3")

	isolatedModCache := filepath.Join(p.Dir(), "isolated-modcache")

	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "fetcher" {
  src {
    path = "./src/svc1"
    exe = "."
  }
  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/fetcher", "-addr", "127.0.0.1:${self.vars.port}"]

    provision "download" {
      cmd = "go mod download %[1]s@%[2]s"
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

service "go" "shared-check" {
  src {
    path = "./src/svc2"
    exe = "."
  }
  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/shared-check", "-addr", "127.0.0.1:${self.vars.port}"]

    provision "assert-shared" {
      after = [service.go.fetcher.runtime.provision.download.success]
      cmd   = "test -f $HOME/%[3]s"
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

service "go" "isolated-check" {
  src {
    path = "./src/svc3"
    exe = "."
  }
  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/isolated-check", "-addr", "127.0.0.1:${self.vars.port}"]

    provision "assert-isolated" {
      after = [service.go.fetcher.runtime.provision.download.success]
      env   = { GOMODCACHE = "%[4]s" }
      cmd   = "! test -f %[4]s/cache/download/%[1]s/@v/%[2]s.zip"
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
`, modulePath, moduleVersion, sharedZipRel, isolatedModCache))

	mustStart(t, p)
	// If any of the three provisions had failed, mustStart would have
	// fataled on a non-zero zordon exit (failfast). Reaching here means
	// all three claims hold: download happened, default cache is shared,
	// per-step GOMODCACHE override genuinely isolates.
}

// TestGoService_buildBarrierFiresBeforeRuntime pins the distinction
// between `service.<tc>.<n>.build.<state>` and `service.<tc>.<n>.runtime.<state>`
// — two separate barriers exposed by every service. A provision wired
// to `build.success` must observe the build's completion as a distinct
// event, BEFORE `runtime.ready`. Without that distinction, the
// build-time barrier would have no purpose: anything depending on it
// would effectively wait for the runtime, defeating the use case
// (running tooling against a freshly built artifact before exposing it).
//
// Asserted via the test:: log order:
//
//	"after-build" fires from a provision gated on first.build.success
//	"after-runtime" fires from a provision gated on first.runtime.ready
//
// Build precedes runtime structurally (you can't be running before
// you're built), so the log MUST contain after-build then after-runtime.
func TestGoService_buildBarrierFiresBeforeRuntime(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/first")
	p.CopyTree("golden/go/echo", "src/second")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "first" {
  src {
    path = "./src/first"
    exe = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/first", "-addr", "127.0.0.1:${self.vars.port}"]
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

service "go" "second" {
  src {
    path = "./src/second"
    exe = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/second", "-addr", "127.0.0.1:${self.vars.port}"]

    provision "after-build" {
      after = [service.go.first.build.success]
      cmd   = test::log("after-build")
    }

    provision "after-runtime" {
      after = [service.go.first.runtime.ready]
      cmd   = test::log("after-runtime")
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
`)

	mustStart(t, p)
	got := p.TestLog()
	idxBuild, idxRuntime := -1, -1
	for i, ln := range got {
		switch ln {
		case "after-build":
			idxBuild = i
		case "after-runtime":
			idxRuntime = i
		}
	}
	if idxBuild < 0 || idxRuntime < 0 {
		t.Fatalf("missing markers in test log: %v", got)
	}
	if idxBuild >= idxRuntime {
		t.Errorf("build.success fired at or after runtime.ready (idx %d vs %d) — barriers may have collapsed: %v",
			idxBuild, idxRuntime, got)
	}
}

// TestGoService_buildFailureBlocksRuntime is the load-bearing negative
// proof of the build barrier: when a service's `build { cmd }` exits
// non-zero, the runtime command MUST NOT be executed. The build phase
// is a hard gate, not advisory — a failed build can't produce the
// artifact the runtime would exec, so starting it anyway is exactly the
// "exit 127, no such file" foot-gun this barrier exists to prevent.
//
// Construction:
//   - build cmd is a forced failure: `sh -c 'exit 7'` (deterministic
//     non-zero code, no toolchain/network involved — it CANNOT flake
//     into success). No `toolchain` block, so the gate under test is
//     reached without a mise/go provisioning detour that could fail
//     first and make "runtime didn't run" vacuously true.
//   - the runtime cmd is a test::log snippet. If — and only if — the
//     barrier were broken and alpha started the runtime regardless, the
//     marker would land in the test log.
//
// Assertions: zordon start fails (failfast on the build error); the
// forced failure actually came from our build cmd (its stderr marker is
// in alpha's log, so we're proving the BUILD gate, not some unrelated
// failure); and the runtime marker NEVER appears.
func TestGoService_buildFailureBlocksRuntime(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  build {
    cmd = ["sh", "-c", "echo zordon-forced-build-failure 1>&2; exit 7"]
  }

  runtime {
    cmd = ["sh", "-c", %s]
  }
}
`, "test::log(\"RUNTIME_STARTED_MUST_NOT_HAPPEN\")"))

	// Failfast tears the bringup down on the build failure: non-zero
	// exit is the expected, correct outcome.
	p.Start(t, zordontest.StartTimeout(5*time.Minute)).Failed()

	for _, ln := range p.TestLog() {
		if ln == "RUNTIME_STARTED_MUST_NOT_HAPPEN" {
			t.Fatalf("runtime cmd executed despite the build failing — build barrier did not gate runtime")
		}
	}

	// Prove the failure was the BUILD (our forced cmd), not something
	// upstream — otherwise "runtime didn't run" would be vacuously true.
	alphaLog, err := os.ReadFile(p.AlphaLogPath())
	if err != nil {
		t.Fatalf("read alpha log %s: %v", p.AlphaLogPath(), err)
	}
	if !strings.Contains(string(alphaLog), "zordon-forced-build-failure") {
		t.Errorf("alpha log lacks the forced build-failure marker — the build cmd may not have run; got:\n%s", alphaLog)
	}
}

// TestGoService_provisionCwdMatchesServiceSrcDir pins the rule that a
// provision shell runs with cwd == its parent service's resolved
// `Runtime.Dir`. Without it, language-native tooling (go run ./cmd/...,
// bundle exec, rails) couldn't locate project manifests relative to the
// checkout, and users would need an explicit `cd` in every provision
// cmd — exactly the foot-gun the rule was added to prevent.
//
// We capture the provision's pwd to a file under fs::tmp(), then
// compare against the service's resolved dir read via `zordon get`.
// Equality is the assertion; the values are both absolute paths
// resolved by alpha at bringup, so they round-trip exactly.
func TestGoService_provisionCwdMatchesServiceSrcDir(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = {
    port    = net::pickport()
    cwdfile = "${fs::tmp()}/cwd.txt"
  }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]

    provision "capture-cwd" {
      cmd = "pwd > ${self.vars.cwdfile}"
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
`)

	mustStart(t, p)
	cwdFile := p.Get(t, "service.go.svc1.vars.cwdfile").String()
	gotCwd, err := readFile(t, cwdFile)
	if err != nil {
		t.Fatalf("read cwd capture %s: %v", cwdFile, err)
	}
	gotCwd = strings.TrimSpace(gotCwd)
	wantDir := p.Get(t, "service.go.svc1.dir").String()
	if gotCwd != wantDir {
		t.Errorf("provision cwd = %q; want service src dir %q", gotCwd, wantDir)
	}
}

// TestGoService_latentProvisionInvokedByPeers pins the exposed-action
// model: a provider declares ONE latent provision (`after = never`) — it
// must never auto-run — and two unrelated services each invoke it for
// themselves by pointing their own provision's `cmd` at it, supplying
// their own TOPIC via env. The provider's snippet appends $TOPIC to a
// shared file whose path is resolved at the PROVIDER's eval time, so all
// invokers write the same file.
//
// Assertions, all from the resulting file:
//   - it exists (both invocations ran),
//   - it contains exactly the two consumer topics (independent runs,
//     each with its own env),
//   - exactly two lines — a third/empty line would mean the latent
//     provision auto-ran with the provider's (empty TOPIC) env.
func TestGoService_latentProvisionInvokedByPeers(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/kafka")
	p.CopyTree("golden/go/echo", "src/app")
	p.CopyTree("golden/go/echo", "src/billing")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "kafka" {
  src {
    path = "./src/kafka"
    exe = "."
  }
  vars = {
    port   = net::pickport()
    topics = "${fs::tmp()}/topics.txt"
  }
  runtime {
    cmd = ["${fs::bin()}/kafka", "-addr", "127.0.0.1:${self.vars.port}"]
    provision "create-topic" {
      after = never
      cmd   = "echo \"$TOPIC\" >> ${self.vars.topics}"
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

service "go" "app" {
  src {
    path = "./src/app"
    exe = "."
  }
  vars = { port = net::pickport() }
  runtime {
    cmd = ["${fs::bin()}/app", "-addr", "127.0.0.1:${self.vars.port}"]
    provision "topic" {
      cmd = service.go.kafka.runtime.provision.create-topic
      env = { TOPIC = "app-events" }
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

service "go" "billing" {
  src {
    path = "./src/billing"
    exe = "."
  }
  vars = { port = net::pickport() }
  runtime {
    cmd = ["${fs::bin()}/billing", "-addr", "127.0.0.1:${self.vars.port}"]
    provision "topic" {
      cmd = service.go.kafka.runtime.provision.create-topic
      env = { TOPIC = "billing-events" }
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
`)

	mustStart(t, p)

	topicsPath := p.Get(t, "service.go.kafka.vars.topics").String()
	data, err := readFile(t, topicsPath)
	if err != nil {
		t.Fatalf("read topics file %s: %v (invocations did not run?)", topicsPath, err)
	}
	lines := strings.Fields(strings.TrimSpace(data))
	sort.Strings(lines)
	want := []string{"app-events", "billing-events"}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Errorf("topics = %v; want %v (a 3rd/empty entry = latent create-topic auto-ran)", lines, want)
	}
}

// TestGoService_nonLatentProvisionIsAlsoInvokable proves `after = never`
// is NOT a prerequisite for being a template: a perfectly ordinary
// provision both auto-runs for its own service AND can be invoked by a
// peer with different env.
//
// The provider's `seed` appends ${WHO:-provider} to a shared file. With
// no env it runs once for the provider → "provider". The consumer
// invokes the same provision with WHO=consumer → "consumer". The file
// must contain exactly those two — one auto-run, one invocation.
func TestGoService_nonLatentProvisionIsAlsoInvokable(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/prov")
	p.CopyTree("golden/go/echo", "src/cons")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "prov" {
  src {
    path = "./src/prov"
    exe = "."
  }
  vars = {
    port = net::pickport()
    f    = "${fs::tmp()}/seeded.txt"
  }
  runtime {
    cmd = ["${fs::bin()}/prov", "-addr", "127.0.0.1:${self.vars.port}"]
    provision "seed" {
      cmd = "echo \"$${WHO:-provider}\" >> ${self.vars.f}"
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

service "go" "cons" {
  src {
    path = "./src/cons"
    exe = "."
  }
  vars = { port = net::pickport() }
  runtime {
    cmd = ["${fs::bin()}/cons", "-addr", "127.0.0.1:${self.vars.port}"]
    provision "seed-mine" {
      after = [service.go.prov.runtime.provision.seed.success]
      cmd   = service.go.prov.runtime.provision.seed
      env   = { WHO = "consumer" }
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
`)

	mustStart(t, p)

	fPath := p.Get(t, "service.go.prov.vars.f").String()
	data, err := readFile(t, fPath)
	if err != nil {
		t.Fatalf("read seeded file %s: %v", fPath, err)
	}
	lines := strings.Fields(strings.TrimSpace(data))
	sort.Strings(lines)
	want := []string{"consumer", "provider"}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Errorf("seeded = %v; want %v (provider auto-run + consumer invocation)", lines, want)
	}
}

// --- shared helpers (package-local) ----------------------------

func mustStart(t *testing.T, p *zordontest.Project) {
	t.Helper()
	p.Start(t).OK()
}

// echoResponse is the JSON shape golden/go/echo's GET / returns.
// Tests that only care about reachability use mustGetEcho; tests
// that inspect specific fields use mustDecodeEcho.
type echoResponse struct {
	Env            map[string]string `json:"env"`
	Cwd            string            `json:"cwd"`
	Argv           []string          `json:"argv"`
	RuntimeVersion string            `json:"runtime_version"`
}

func mustGetEcho(t *testing.T, port int) {
	t.Helper()
	echo := mustDecodeEcho(t, port)
	if !strings.HasPrefix(echo.RuntimeVersion, "go1.26") {
		t.Errorf("runtime_version = %q; want go1.26.x (pinned toolchain)", echo.RuntimeVersion)
	}
}

func mustDecodeEcho(t testing.TB, port int) echoResponse {
	t.Helper()
	// Retry: readiness having passed means the service served alpha's probe,
	// but the test connects from a different process a beat later — on a
	// loaded CI runner the accept loop can briefly refuse, or a slow runtime
	// (rust cold-build) can lag the first external request. A genuinely dead
	// service stays refused for the whole window, so this masks the race, not
	// real failures.

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	for {
		echo, err := tryDecodeEcho(ctx, url)
		if err == nil {
			return echo
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for echo to start: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func tryDecodeEcho(ctx context.Context, url string) (echoResponse, error) {
	var echo echoResponse
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return echo, err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return echo, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return echo, err
	}
	if res.StatusCode != http.StatusOK {
		return echo, fmt.Errorf("status %d: %s", res.StatusCode, body)
	}
	if err := json.NewDecoder(strings.NewReader(string(body))).Decode(&echo); err != nil {
		return echo, fmt.Errorf("decode echo: %w (body: %s)", err, body)
	}

	return echo, nil
}

// readFile is a thin testing.T-aware wrapper that returns content
// as string for tests asserting on generated-file bodies.
func readFile(t *testing.T, path string) (string, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
