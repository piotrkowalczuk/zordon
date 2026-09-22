//go:build conformance_rust

package conformance_test

import (
	"fmt"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// mise's rust backend defaults its cargo/rustup homes to ~/.cargo and
// ~/.rustup, so a developer's ~/.cargo/config.toml used to be part of
// every zordon rust build. Here it carries a rustflag rustc rejects, and
// the host CARGO_HOME / RUSTUP_HOME / RUSTFLAGS are poisoned: a green
// `cargo install` proves the toolchain lives under ZORDON_HOME and reads
// none of it, and nothing is written back into HOME.
func TestRustService_hermetic_hostConfigIgnored(t *testing.T) {
	home := poisonedHome(t,
		homeFile{".cargo/config.toml", "[build]\nrustflags = [\"--poison\"]\n"},
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
    cmd = ["${fs::bin()}/echo", "--addr", "127.0.0.1:${self.vars.port}"]
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
		"CARGO_HOME":  poisonSentinel + "/cargo",
		"RUSTUP_HOME": poisonSentinel + "/rustup",
		"RUSTFLAGS":   "--poison",
	})

	port := p.Get(t, "service.rust.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	assertNoPoison(t, echo.Env, "RUSTFLAGS")
	assertUnderZordonHome(t, p, echo.Env, "CARGO_HOME")
	assertUnderZordonHome(t, p, echo.Env, "RUSTUP_HOME")
	assertHomeUntouched(t, home, ".cargo/bin", ".cargo/registry", ".rustup")
}
