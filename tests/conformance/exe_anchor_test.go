package conformance_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Exe-anchor conformance: pin the rule that `<src.path>/<exe>` is the
// unified working directory for both build and runtime, across every
// toolchain. The fixtures are arranged so `exe ≠ "."`:
//
//	<project>/Alphasfile
//	<project>/src/cmd/echo/{main.go, Cargo.toml, package.json}
//
// `src.path = "./src"` + `exe = "./cmd/echo"` — the module manifest
// (go.mod / Cargo.toml / package.json) lives at `<src>/cmd/echo/`,
// NOT at `<src>/`. The old behavior built from cwd = `<src>` with
// `./cmd/echo` as a path argument; for Go that means running
// `go build ./cmd/echo` from a dir with no go.mod, which fails.
// The unified behavior builds from cwd = `<src>/cmd/echo` where
// the manifest sits, so `go build .` / `cargo install --path .` /
// `npm install` resolve naturally.
//
// The runtime assertion (echoed cwd ends with the exe offset) is
// the cleanest negative test for the old behavior: there, runtime
// cwd was `<dest>` and the suffix wouldn't match.

func TestExeAnchor_Go(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/cmd/echo")

	const port = 27690
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "echo" {
  src {
    path = "./src"
    exe  = "./cmd/echo"
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
`, port))

	mustStart(t, p)
	echo := mustDecodeEcho(t, port)
	if !strings.HasPrefix(echo.RuntimeVersion, "go1.26") {
		t.Errorf("runtime_version = %q; want go1.26.x (pinned toolchain)", echo.RuntimeVersion)
	}
	assertCwdEndsWithExe(t, echo.Cwd, "src", "cmd", "echo")
}

func TestExeAnchor_Rust(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/cmd/echo")

	const port = 27790
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  rust {
    version = "%s"
  }
}

service "rust" "echo" {
  src {
    path = "./src"
    exe  = "./cmd/echo"
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
	echo := mustDecodeEcho(t, port)
	if !strings.Contains(echo.RuntimeVersion, rustVersion) {
		t.Errorf("runtime_version = %q; want it to contain pinned rust %s", echo.RuntimeVersion, rustVersion)
	}
	assertCwdEndsWithExe(t, echo.Cwd, "src", "cmd", "echo")
}

func TestExeAnchor_Nodejs(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/nodejs/echo", "src/cmd/echo")

	const port = 28790
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
  }
}

service "nodejs" "echo" {
  src {
    path = "./src"
    exe  = "./cmd/echo"
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
	echo := mustDecodeEcho(t, port)
	if !strings.HasPrefix(echo.RuntimeVersion, "v22.") {
		t.Errorf("runtime_version = %q; want v22.x (pinned toolchain)", echo.RuntimeVersion)
	}
	assertCwdEndsWithExe(t, echo.Cwd, "src", "cmd", "echo")
}

func assertCwdEndsWithExe(t *testing.T, cwd string, parts ...string) {
	t.Helper()
	want := filepath.Join(parts...)
	if !strings.HasSuffix(cwd, want) {
		t.Errorf("service cwd = %q; want a path ending in %q (build/runtime should anchor on `<src.path>/<exe>`, not just `<src.path>`)",
			cwd, want)
	}
}
