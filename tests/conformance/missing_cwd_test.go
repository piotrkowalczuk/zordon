package conformance_test

import (
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Regression for issue #44: a service whose in-place src{exe} subdir no
// longer exists must be reported as a clear config error naming the exe —
// NOT as the misleading `fork/exec <toolchain go binary>: no such file or
// directory` that os/exec produces when it spawns a build with a cwd that
// does not exist. `path = "."` is the project root (it exists), while
// `./cmd/nope` was never created, so the exe-anchored working dir is gone.

const missingExeAlphasfile = `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

service "go" "app" {
  src {
    path = "."
    exe  = "./cmd/nope"
  }

  runtime {
    cmd = ["${fs::bin()}/app"]
  }
}
`

func TestMissingExe_planReportsClearError(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", missingExeAlphasfile)

	res := p.Zordon("plan").Run(t)
	if res.ExitCode == 0 {
		t.Fatalf("zordon plan unexpectedly succeeded with a removed exe subdir\nstdout: %s\nstderr: %s",
			res.Stdout, res.Stderr)
	}
	assertMissingExeError(t, res.Stdout+res.Stderr)
}

func TestMissingExe_startReportsClearError(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", missingExeAlphasfile)

	// Fails during config resolution (validateInPlaceSources), before any
	// alpha is spawned or any build runs — so no toolchain install and no
	// leftover supervisor.
	res := p.Zordon("start").Run(t)
	if res.ExitCode == 0 {
		t.Fatalf("zordon start unexpectedly succeeded with a removed exe subdir\nstdout: %s\nstderr: %s",
			res.Stdout, res.Stderr)
	}
	assertMissingExeError(t, res.Stdout+res.Stderr)
}

func assertMissingExeError(t *testing.T, combined string) {
	t.Helper()
	for _, want := range []string{"app", "./cmd/nope", "does not exist"} {
		if !strings.Contains(combined, want) {
			t.Errorf("error should name the service and the missing exe (%q); got:\n%s", want, combined)
		}
	}
	if strings.Contains(combined, "fork/exec") {
		t.Errorf("error must not misreport as fork/exec against the toolchain binary; got:\n%s", combined)
	}
}
