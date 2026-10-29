//go:build conformance_go

package conformance_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// A developer's Go config never reaches a zordon build: the user env file
// (`go env -w`, read from the platform config dir under HOME), ~/.netrc
// and the host env are all poisoned with the exports developers really
// carry around, each chosen so that being read fails the build or yields a
// binary that cannot run — `-mod=vendor` against a module with no vendor
// dir is a hard error, `GO111MODULE=off` makes a module build GOPATH-mode,
// `GOOS=linux` (a cross-compile leftover) yields a binary macOS/Linux
// x86 cannot exec, a stale `GOROOT` from another install breaks the
// standard library lookup, `GOTOOLCHAIN=go1.0` would fetch a toolchain
// that does not exist, and a shell-activated version manager (`MISE_*`,
// `GVM_ROOT`) would redirect where the toolchain is looked up.
func TestGoService_hermetic_hostConfigIgnored(t *testing.T) {
	home := poisonedHome(t,
		homeFile{".config/go/env", "GOFLAGS=-mod=vendor\nGOPROXY=http://127.0.0.1:1\n"},
		homeFile{"Library/Application Support/go/env", "GOFLAGS=-mod=vendor\nGOPROXY=http://127.0.0.1:1\n"},
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
		"GOFLAGS":       "-mod=vendor",
		"GOPROXY":       "http://127.0.0.1:1" + poisonSentinel,
		"GONOSUMDB":     "*",
		"GOPRIVATE":     "*",
		"GOTOOLCHAIN":   "go1.0",
		"GO111MODULE":   "off",
		"GOOS":          "linux",
		"GOARCH":        "mips",
		"CGO_ENABLED":   "0",
		"GOROOT":        poisonSentinel + "/goroot",
		"GOPATH":        poisonSentinel + "/gopath",
		"GOBIN":         poisonSentinel + "/gobin",
		"GOMODCACHE":    poisonSentinel + "/modcache",
		"GOCACHE":       poisonSentinel + "/gocache",
		"GOENV":         poisonSentinel + "/goenv",
		"GOWORK":        poisonSentinel + "/go.work",
		"MISE_DATA_DIR": poisonSentinel + "/mise",
		"MISE_SHELL":    "zsh",
		"GVM_ROOT":      poisonSentinel + "/gvm",
	})

	port := p.Get(t, "service.go.svc1.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	assertNoPoison(t, echo.Env, "GOFLAGS", "GOPROXY", "GONOSUMDB", "GOPRIVATE", "GO111MODULE", "GOOS", "GOARCH", "CGO_ENABLED", "GOWORK", "MISE_DATA_DIR", "MISE_SHELL", "GVM_ROOT")
	if !strings.HasPrefix(echo.Env["GOROOT"], filepath.Join(p.Home(), "toolchain", "installs", "go")) {
		t.Errorf("GOROOT = %q, want the mise install", echo.Env["GOROOT"])
	}
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
		"HTTPS_PROXY":     "http://127.0.0.1:1" + poisonSentinel,
		"HTTP_PROXY":      "http://127.0.0.1:1" + poisonSentinel,
		"ALL_PROXY":       "socks5://127.0.0.1:1" + poisonSentinel,
		"ASDF_DATA_DIR":   poisonSentinel,
		"ASDF_DIR":        poisonSentinel + "/asdf",
		"MISE_DATA_DIR":   poisonSentinel + "/mise",
		"MISE_CONFIG_DIR": poisonSentinel + "/mise-config",
		"AQUA_ROOT_DIR":   poisonSentinel + "/aqua",
	})

	dump := p.Get(t, "service.go.app.vars.dump").String()
	body, err := readFile(t, dump)
	if err != nil {
		t.Fatalf("provision env dump: %v", err)
	}
	assertNoPoison(t, envFromDump(body), "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "ASDF_DATA_DIR", "ASDF_DIR", "MISE_DATA_DIR", "MISE_CONFIG_DIR", "AQUA_ROOT_DIR")
}
