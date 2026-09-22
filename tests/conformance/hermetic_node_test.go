//go:build conformance_node

package conformance_test

import (
	"fmt"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// ~/.npmrc is where registries, auth tokens and per-scope settings live,
// and npm/pnpm read it from HOME. Poisoned here with what developers
// really keep there — a script-shell that does not exist (so `npm start`,
// the inferred runtime cmd, cannot spawn if the file is read), a dead
// registry with an auth token, a global prefix — and ~/.yarnrc with a dead
// registry. The host env carries the exports that break Node builds in
// practice: `NODE_ENV=production` (npm skips devDependencies), a bogus
// `NODE_OPTIONS` (node refuses to start), `NODE_PATH` and `NODE_EXTRA_CA_CERTS`
// pointing nowhere, an nvm/volta/mise activation, `npm_config_*` lowercase
// overrides and a corepack home redirect. A green bringup proves the user
// config was relocated, and the caches (npm, corepack) never touch HOME.
func TestNodeService_hermetic_hostConfigIgnored(t *testing.T) {
	home := poisonedHome(t,
		homeFile{".npmrc", "script-shell=/poison/sh\nregistry=http://127.0.0.1:1\n//127.0.0.1:1/:_authToken=poison\nprefix=/poison/npm-global\n"},
		homeFile{".yarnrc", "registry \"http://127.0.0.1:1\"\n"},
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
		"NODE_ENV":                "production",
		"NODE_OPTIONS":            "--poison",
		"NODE_PATH":               poisonSentinel + "/node_modules",
		"NODE_EXTRA_CA_CERTS":     poisonSentinel + "/ca.pem",
		"NPM_CONFIG_REGISTRY":     "http://127.0.0.1:1" + poisonSentinel,
		"NPM_CONFIG_PREFIX":       poisonSentinel + "/npm",
		"NPM_CONFIG_USERCONFIG":   poisonSentinel + "/npmrc",
		"NPM_CONFIG_CACHE":        poisonSentinel + "/npm-cache",
		"NPM_CONFIG_SCRIPT_SHELL": poisonSentinel + "/sh",
		"npm_config_registry":     "http://127.0.0.1:1" + poisonSentinel,
		"npm_config_script_shell": poisonSentinel + "/sh",
		"COREPACK_HOME":           poisonSentinel + "/corepack",
		"COREPACK_NPM_REGISTRY":   "http://127.0.0.1:1" + poisonSentinel,
		"NVM_DIR":                 poisonSentinel + "/nvm",
		"NVM_BIN":                 poisonSentinel + "/nvm/bin",
		"VOLTA_HOME":              poisonSentinel + "/volta",
		"MISE_DATA_DIR":           poisonSentinel + "/mise",
	})

	port := p.Get(t, "service.nodejs.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	assertNoPoison(t, echo.Env, "NODE_ENV", "NODE_OPTIONS", "NODE_PATH", "NODE_EXTRA_CA_CERTS", "NPM_CONFIG_REGISTRY", "NPM_CONFIG_PREFIX", "NPM_CONFIG_SCRIPT_SHELL", "npm_config_registry", "npm_config_script_shell", "COREPACK_NPM_REGISTRY", "NVM_DIR", "NVM_BIN", "VOLTA_HOME", "MISE_DATA_DIR")
	assertUnderZordonHome(t, p, echo.Env, "NPM_CONFIG_USERCONFIG")
	assertUnderZordonHome(t, p, echo.Env, "NPM_CONFIG_CACHE")
	assertUnderZordonHome(t, p, echo.Env, "COREPACK_HOME")
	assertHomeUntouched(t, home, ".npm", ".cache/node/corepack")
}
