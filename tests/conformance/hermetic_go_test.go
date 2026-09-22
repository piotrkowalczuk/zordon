//go:build conformance_go

package conformance_test

import (
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// A developer's Go config never reaches a zordon build: the user env file
// (`go env -w`, read from the platform config dir under HOME), ~/.netrc
// and the host GOFLAGS/GOPROXY/GOTOOLCHAIN are all poisoned so that any of
// them being read fails the build — `-mod=vendor` against a module with
// no vendor dir is a hard error, `GOTOOLCHAIN=go1.0` would try to fetch a
// toolchain that does not exist.
func TestGoService_hermetic_hostConfigIgnored(t *testing.T) {
	home := poisonedHome(t,
		homeFile{".config/go/env", "GOFLAGS=-mod=vendor\n"},
		homeFile{"Library/Application Support/go/env", "GOFLAGS=-mod=vendor\n"},
		homeFile{".netrc", "machine proxy.golang.org login poison password poison\n"},
	)

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
    failure_threshold = 100
  }
}
`)

	startPoisoned(t, p, home, map[string]string{
		"GOFLAGS":     "-mod=vendor",
		"GOPROXY":     "http://127.0.0.1:1" + poisonSentinel,
		"GOTOOLCHAIN": "go1.0",
	})

	port := p.Get(t, "service.go.svc1.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	assertNoPoison(t, echo.Env, "GOFLAGS", "GOPROXY")
	if echo.Env["GOENV"] != "off" {
		t.Errorf("GOENV = %q, want off (the user env file must be ignored)", echo.Env["GOENV"])
	}
	if echo.Env["GOTOOLCHAIN"] != "local" {
		t.Errorf("GOTOOLCHAIN = %q, want local", echo.Env["GOTOOLCHAIN"])
	}
	assertUnderZordonHome(t, p, echo.Env, "GOCACHE")
	assertUnderZordonHome(t, p, echo.Env, "NETRC")
	assertHomeUntouched(t, home, ".cache/go-build", "Library/Caches/go-build", "go/pkg")
}

// The pkg pseudo-toolchain goes through the same `mise install` path: a
// poisoned proxy would sink the aqua download of atlas, and the provision
// that runs it must see none of the host poison.
func TestPkgToolchain_hermetic_hostEnvIgnored(t *testing.T) {
	home := poisonedHome(t, homeFile{".asdfrc", "legacy_version_file = yes\n"})

	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/app")
	p.CopyTree("examples/pkg_tools/migrations", "src/app/migrations")
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]

toolchain {
  go {
    version = "1.26.2"
  }
  pkg {
    tools = { "aqua:ariga/atlas" = "1.2.0" }
  }
}

service "go" "app" {
  src {
    path = "./src/app"
    exe  = "."
  }

  vars = {
    port = net::pickport()
    db   = "${fs::state()}/app.db"
    dump = "${fs::state()}/migrate-env.txt"
  }

  runtime {
    after = [self.runtime.provision.migrate.success]

    provision "migrate" {
      env = { PATH = env::prepend(fs::toolchain::bin(toolchain.pkg)) }
      cmd = "env > ${self.vars.dump} && atlas migrate apply --url sqlite://${self.vars.db}?_fk=1 --dir file://${fs::src()}/migrations"
    }

    cmd = ["${fs::bin()}/app", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 100
  }
}
`)

	startPoisoned(t, p, home, map[string]string{
		"HTTPS_PROXY":   "http://127.0.0.1:1" + poisonSentinel,
		"HTTP_PROXY":    "http://127.0.0.1:1" + poisonSentinel,
		"ASDF_DATA_DIR": poisonSentinel,
	})

	dump := p.Get(t, "service.go.app.vars.dump").String()
	body, err := readFile(t, dump)
	if err != nil {
		t.Fatalf("provision env dump: %v", err)
	}
	assertNoPoison(t, envFromDump(body), "HTTPS_PROXY", "HTTP_PROXY", "ASDF_DATA_DIR")
}
