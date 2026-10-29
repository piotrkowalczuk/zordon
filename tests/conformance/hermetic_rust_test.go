//go:build conformance_rust

package conformance_test

import (
	"fmt"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// mise's rust backend defaults its cargo/rustup homes to ~/.cargo and
// ~/.rustup, so a developer's ~/.cargo/config.toml used to be part of
// every zordon rust build. Here it carries the config developers really
// keep there — a rustflag rustc rejects, a registry mirror on a dead port,
// a wrapper binary that does not exist — plus a credentials file, and the
// host env carries the exports that break builds in practice: a rustup
// override to a toolchain that is not installed, `RUSTC_WRAPPER=sccache`
// without sccache, a cross `CARGO_BUILD_TARGET`, a redirected
// `CARGO_TARGET_DIR` (which would move the artifact away from fs::bin),
// and a shell-activated mise. A green `cargo install` proves the
// toolchain lives under ZORDON_HOME and reads none of it, and nothing is
// written back into HOME.
func TestRustService_hermetic_hostConfigIgnored(t *testing.T) {
	home := poisonedHome(t,
		homeFile{".cargo/config.toml", `[build]
rustflags = ["--poison"]
rustc-wrapper = "/poison/sccache"

[source.crates-io]
replace-with = "poison"

[source.poison]
registry = "http://127.0.0.1:1/index"
`},
		homeFile{".cargo/credentials.toml", "[registry]\ntoken = \"poison\"\n"},
	)

	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/echo")
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

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/echo", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 300
  }
}
`, rustVersion))

	startPoisoned(t, p, home, map[string]string{
		"CARGO_HOME":                          poisonSentinel + "/cargo",
		"RUSTUP_HOME":                         poisonSentinel + "/rustup",
		"RUSTUP_TOOLCHAIN":                    "nightly-1970-01-01",
		"RUSTFLAGS":                           "--poison",
		"RUSTDOCFLAGS":                        "--poison",
		"RUSTC_WRAPPER":                       poisonSentinel + "/sccache",
		"RUSTC":                               poisonSentinel + "/rustc",
		"CARGO_BUILD_TARGET":                  "wasm32-unknown-unknown",
		"CARGO_TARGET_DIR":                    poisonSentinel + "/target",
		"CARGO_BUILD_JOBS":                    "0",
		"CARGO_NET_OFFLINE":                   "true",
		"CARGO_REGISTRIES_CRATES_IO_PROTOCOL": "git",
		"MISE_DATA_DIR":                       poisonSentinel + "/mise",
		"MISE_CARGO_HOME":                     poisonSentinel + "/mise-cargo",
	})

	port := p.Get(t, "service.rust.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	assertNoPoison(t, echo.Env, "RUSTFLAGS", "RUSTDOCFLAGS", "RUSTC_WRAPPER", "RUSTC", "CARGO_BUILD_TARGET", "CARGO_TARGET_DIR", "CARGO_BUILD_JOBS", "CARGO_NET_OFFLINE", "CARGO_REGISTRIES_CRATES_IO_PROTOCOL", "MISE_DATA_DIR", "MISE_CARGO_HOME")
	// mise pins RUSTUP_TOOLCHAIN itself, so the key is legitimately present;
	// what must not survive is the host's override.
	if got := echo.Env["RUSTUP_TOOLCHAIN"]; got != rustVersion {
		t.Errorf("RUSTUP_TOOLCHAIN = %q, want the mise pin %s", got, rustVersion)
	}
	assertUnderZordonHome(t, p, echo.Env, "CARGO_HOME")
	assertUnderZordonHome(t, p, echo.Env, "RUSTUP_HOME")
	assertHomeUntouched(t, home, ".cargo/bin", ".cargo/registry", ".rustup")
}
