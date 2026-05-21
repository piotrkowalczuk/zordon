package conformance_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Nodejs conformance: one test per `service "nodejs" { ... }` oneof
// choice, plus table-driven coverage of the two filesystem-detected
// axes (PM and runtime inference) that go/rust don't have.
//
// Fixtures live in golden/nodejs/echo* — per-PM and per-inference-tier
// dirs so the test body stays declarative (CopyTree alone configures
// the cell). Wire shape matches golden/{go,rust}/echo, reusing
// mustDecodeEcho from go_test.go.

const nodeVersion = "22.11.0"

// Canonical: src + default build + scripts.start inference + HTTP probe.
func TestNodeService_srcDefault_inferredScriptsStart(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/nodejs/echo", "src/echo")

	const port = 28700
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
  }
}

service "nodejs" "echo" {
  src = "./src/echo"

  vars = { port = %d }
  env  = { PORT = "${self.vars.port}" }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, nodeVersion, port))

	mustStart(t, p)
	mustGetNodeEcho(t, port)
}

// Explicit `build { cmd }` overrides defaultBuild; `true` no-ops the
// install for the zero-dep fixture.
func TestNodeService_srcExplicitBuildCmd(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/nodejs/echo", "src/echo")

	const port = 28702
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
  }
}

service "nodejs" "echo" {
  src = "./src/echo"

  build {
    cmd = ["true"]
  }

  vars = { port = %d }
  env  = { PORT = "${self.vars.port}" }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, nodeVersion, port))

	mustStart(t, p)
	mustGetNodeEcho(t, port)
}

// Explicit `runtime { cmd }` bypasses inferNodeRunCmd; a regression
// flipping precedence breaks this since scripts.start would fire and
// strip the -addr argv.
func TestNodeService_srcExplicitRuntimeCmd(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/nodejs/echo", "src/echo")

	const port = 28703
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
  }
}

service "nodejs" "echo" {
  src = "./src/echo"

  vars = { port = %d }

  runtime {
    cmd = ["node", "app.js", "-addr", "127.0.0.1:${self.vars.port}"]
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
`, nodeVersion, port))

	mustStart(t, p)
	mustGetNodeEcho(t, port)
}

// No readiness block → stabilization timer fallback.
func TestNodeService_noReadinessUsesStabilization(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/nodejs/echo", "src/echo")

	const port = 28704
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
  }
}

service "nodejs" "echo" {
  src = "./src/echo"

  vars = { port = %d }
  env  = { PORT = "${self.vars.port}" }
}
`, nodeVersion, port))

	mustStart(t, p)
	time.Sleep(2 * time.Second) // ~1s stabilization + slack
	mustGetNodeEcho(t, port)
}

// Service `env {}` must survive the npm→node hop (extra layer vs
// go/rust). echo mirrors process.env back.
func TestNodeService_envBlockReachesRuntime(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/nodejs/echo", "src/echo")

	const port = 28705
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
  }
}

service "nodejs" "echo" {
  src = "./src/echo"

  vars = { port = %d }
  env  = {
    PORT     = "${self.vars.port}"
    GREETING = "hello-from-env-block"
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
`, nodeVersion, port))

	mustStart(t, p)
	echo := mustDecodeEcho(t, port)
	if got := echo.Env["GREETING"]; got != "hello-from-env-block" {
		t.Errorf("GREETING in service env = %q; want %q", got, "hello-from-env-block")
	}
}

// PM auto-detection in defaultBuild + runtime inference. Each cell
// asserts the right install + start commands appear in alpha's log
// AND that the install actually succeeded (service comes up).
func TestNodeService_pmDetect_table(t *testing.T) {
	cases := map[string]struct {
		port           int
		goldenDir      string
		toolchainTools string // body of `tools = { ... }`; "" = no tools block
		wantBuild      string // install cmd substring in alpha log
		wantRun        string // runtime cmd substring in alpha log
	}{
		"npm-fallback": {
			port:      28710,
			goldenDir: "golden/nodejs/echo",
			wantBuild: "npm install",
			wantRun:   "npm start",
		},
		"npm-ci": {
			port:      28711,
			goldenDir: "golden/nodejs/echo-npm-ci",
			wantBuild: "npm ci",
			wantRun:   "npm start",
		},
		"pnpm-field": {
			// pnpm 9.x pinned via packageManager field; 10/11 raise the
			// Node floor — see internal/tools/corepack_test.go.
			port:      28712,
			goldenDir: "golden/nodejs/echo-pnpm",
			wantBuild: "pnpm install",
			wantRun:   "pnpm start",
		},
		"yarn-field": {
			port:      28713,
			goldenDir: "golden/nodejs/echo-yarn",
			wantBuild: "yarn install",
			wantRun:   "yarn start",
		},
		"bun-field": {
			// bun isn't shimmed by Corepack; install via mise tools.
			port:           28714,
			goldenDir:      "golden/nodejs/echo-bun",
			toolchainTools: `tools = { bun = "1.1.42" }`,
			wantBuild:      "bun install",
			wantRun:        "bun start",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			p := zordontest.NewProject(t)
			p.CopyTree(c.goldenDir, "src/echo")

			toolsLine := ""
			if c.toolchainTools != "" {
				toolsLine = "    " + c.toolchainTools + "\n"
			}
			p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
%s  }
}

service "nodejs" "echo" {
  src = "./src/echo"

  vars = { port = %d }
  env  = { PORT = "${self.vars.port}" }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, nodeVersion, toolsLine, c.port))

			mustStart(t, p)
			mustGetNodeEcho(t, c.port)

			log := p.AlphaLog()
			if !log.Contains(c.wantBuild) {
				t.Errorf("alpha log lacks %q — PM detection picked the wrong build", c.wantBuild)
			}
			if !log.Contains(c.wantRun) {
				t.Errorf("alpha log lacks %q — runtime inference didn't use the detected PM", c.wantRun)
			}
		})
	}
}

// Runtime inference precedence: scripts.start → main → bin. One cell
// per tier so a regression in tier N+1 isn't masked by tier N firing.
func TestNodeService_runtimeInference_table(t *testing.T) {
	cases := map[string]struct {
		port      int
		goldenDir string
		wantRun   string // runtime cmd substring in alpha log
	}{
		"scripts-start-wins": {
			// base golden has scripts.start AND main; scripts wins.
			port:      28720,
			goldenDir: "golden/nodejs/echo",
			wantRun:   "npm start",
		},
		"main-when-no-scripts-start": {
			port:      28721,
			goldenDir: "golden/nodejs/echo-infer-main",
			wantRun:   "node app.js",
		},
		"bin-string-when-no-main": {
			port:      28722,
			goldenDir: "golden/nodejs/echo-infer-bin-string",
			wantRun:   "node ./app.js",
		},
		"bin-single-map-when-no-main": {
			port:      28723,
			goldenDir: "golden/nodejs/echo-infer-bin-map",
			wantRun:   "node ./app.js",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			p := zordontest.NewProject(t)
			p.CopyTree(c.goldenDir, "src/echo")

			p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
  }
}

service "nodejs" "echo" {
  src = "./src/echo"

  vars = { port = %d }
  env  = { PORT = "${self.vars.port}" }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, nodeVersion, c.port))

			mustStart(t, p)
			mustGetNodeEcho(t, c.port)

			log := p.AlphaLog()
			if !log.Contains(c.wantRun) {
				t.Errorf("alpha log lacks %q — runtime inference picked the wrong tier", c.wantRun)
			}
		})
	}
}

// git source oneof — same as rust_test: requires a published
// github/gitlab/bitbucket repo, no offline path.
func TestNodeService_gitSource(t *testing.T) {
	t.Skip("git source oneof requires a network clone from a supported host; no offline path")
}

// nodejs has no use-only mode in v1 — see docs/services/nodejs.md.
func TestNodeService_useOnly(t *testing.T) {
	t.Skip("nodejs has no use-only mode in v1; deferred")
}

// mustGetNodeEcho confirms reachability AND that process.version
// contains the mise-pinned node version.
func mustGetNodeEcho(t *testing.T, port int) {
	t.Helper()
	echo := mustDecodeEcho(t, port)
	if !strings.Contains(echo.RuntimeVersion, nodeVersion) {
		t.Errorf("runtime_version = %q; want it to contain pinned node %s", echo.RuntimeVersion, nodeVersion)
	}
}
