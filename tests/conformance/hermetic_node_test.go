//go:build conformance_node

package conformance_test

import (
	"fmt"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// ~/.npmrc is where registries, auth tokens and per-scope settings live,
// and npm/pnpm read it from HOME. Poisoned here with a script-shell that
// does not exist — so `npm start` (the inferred runtime cmd) cannot spawn
// if the file is read — and a dead registry, plus host NPM_CONFIG_* and
// NODE_OPTIONS poison. A green bringup proves the user config was
// relocated, and the caches (npm, corepack) never touch HOME.
func TestNodeService_hermetic_hostConfigIgnored(t *testing.T) {
	home := poisonedHome(t,
		homeFile{".npmrc", "script-shell=/poison/sh\nregistry=http://127.0.0.1:1\n"},
	)

	p := zordontest.NewProject(t)
	p.CopyTree("golden/nodejs/echo", "src/echo")
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
  }
}

service "nodejs" "echo" {
  src { path = "./src/echo" }

  vars = { port = net::pickport() }
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
`, nodeVersion))

	startPoisoned(t, p, home, map[string]string{
		"NPM_CONFIG_REGISTRY": "http://127.0.0.1:1" + poisonSentinel,
		"NPM_CONFIG_PREFIX":   poisonSentinel + "/npm",
		"NODE_OPTIONS":        "--poison",
	})

	port := p.Get(t, "service.nodejs.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	assertNoPoison(t, echo.Env, "NPM_CONFIG_REGISTRY", "NPM_CONFIG_PREFIX", "NODE_OPTIONS")
	assertUnderZordonHome(t, p, echo.Env, "NPM_CONFIG_USERCONFIG")
	assertUnderZordonHome(t, p, echo.Env, "NPM_CONFIG_CACHE")
	assertUnderZordonHome(t, p, echo.Env, "COREPACK_HOME")
	assertHomeUntouched(t, home, ".npm", ".cache/node/corepack")
}
