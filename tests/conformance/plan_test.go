// Plan conformance: `zordon plan` resolves the chain statically (no
// alpha spawn, no build) and renders the Alphasfile back as HCL with
// every interpolation already substituted by its concrete value. The
// contract under test:
//
//   - exit 0 on success;
//   - output is HCL re-render — every `${...}`, `self.*`, `service.X.Y`,
//     `net::pickport()`, `fs::*` etc. gone, replaced by literals;
//     cross-service refs resolve to the producer's actual values;
//   - a fresh `pickport` allocation produces a concrete integer port;
//   - no alpha required (this is the "preflight" command);
//   - unresolvable expressions abort with non-zero exit, no partial
//     dump.
//
// These invariants protect users who run `plan` to *prove* a config
// can resolve before committing to a real `start`.
package conformance_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// alphaLogWritten reports whether the alpha log exists and has any
// bytes — proxy for "did plan spawn alpha?". A zero-byte file from
// some earlier touch wouldn't constitute an alpha run.
func alphaLogWritten(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.Size() > 0
}

func TestPlan_resolvesInterpolationsToConcreteValues(t *testing.T) {
	p := zordontest.NewProject(t)

	// Two services with a cross-service ref + builtins:
	//   - `db`     allocates its own port via net::pickport()
	//   - `app`    interpolates the db's port into env + arguments + cmd
	//   - both reference fs::tmp() (per-invocation tmp dir)
	// No source { path }, no `runtime { cmd }` running real binaries —
	// `plan` is pure resolution, doesn't build or spawn.
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

service "go" "db" {
  package = "example.com/db@v0.0.0"

  vars = {
    port = net::pickport()
  }

  arguments = {
    listen = "127.0.0.1:${self.vars.port}"
  }
}

service "go" "app" {
  package = "example.com/app@v0.0.0"

  vars = {
    port    = net::pickport()
    db_addr = "127.0.0.1:${service.go.db.vars.port}"
  }

  env = {
    DB_URL  = "postgres://${self.vars.db_addr}/x"
    TMP_DIR = fs::tmp()
  }

  arguments = {
    addr = "127.0.0.1:${self.vars.port}"
    db   = self.vars.db_addr
  }
}
`)

	res := p.Zordon("plan").Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon plan: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	out := res.Stdout

	// Every interpolation token must be gone. `${`, `self.`, `service.`,
	// `net::`, `fs::` are the surface tokens users type — none survives
	// resolution.
	for _, forbidden := range []string{"${", "self.", "service.go.db.vars", "net::", "fs::"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("plan output still contains unresolved token %q\n----- output -----\n%s\n-----",
				forbidden, out)
		}
	}

	// Two distinct concrete ports must appear (db.port and app.port,
	// both from net::pickport). Extract them and verify they're (a)
	// numeric, (b) different from each other, (c) cross-referenced
	// correctly (db_addr literally contains db.port).
	portRE := regexp.MustCompile(`(?m)^\s*port\s*=\s*(\d+)\s*$`)
	matches := portRE.FindAllStringSubmatch(out, -1)
	if len(matches) < 2 {
		t.Fatalf("expected at least 2 concrete `port = <int>` lines, got %d\n%s",
			len(matches), out)
	}
	dbPort, appPort := matches[0][1], matches[1][1]
	if dbPort == appPort {
		t.Errorf("db.port and app.port both = %s; net::pickport should hand out distinct ports", dbPort)
	}

	// The cross-service ref `service.go.db.vars.port` must have been
	// inlined into app's db_addr / DB_URL / arguments.db.
	wantDBAddr := "127.0.0.1:" + dbPort
	if !strings.Contains(out, wantDBAddr) {
		t.Errorf("expected cross-service ref to resolve to %q in app's env/arguments\n%s",
			wantDBAddr, out)
	}
	// And it must appear on a db_addr line specifically (not just by
	// accident inside the db block).
	dbAddrRE := regexp.MustCompile(`(?m)^\s*db_addr\s*=\s*"127\.0\.0\.1:` + regexp.QuoteMeta(dbPort) + `"\s*$`)
	if !dbAddrRE.MatchString(out) {
		t.Errorf("expected line `db_addr = \"127.0.0.1:%s\"` in plan output\n%s", dbPort, out)
	}

	// Sanity: TMP_DIR resolves to an absolute path (fs::tmp() returns
	// the per-invocation tmp dir under $TMPDIR).
	tmpRE := regexp.MustCompile(`(?m)^\s*TMP_DIR\s*=\s*"(/[^"]+)"\s*$`)
	tmpMatch := tmpRE.FindStringSubmatch(out)
	if tmpMatch == nil {
		t.Errorf("expected `TMP_DIR = \"/abs/path\"` in plan output\n%s", out)
	}

	// Plan must NOT have spawned alpha — it's a static, read-only op.
	// Cheapest check: no alpha log was written.
	if alphaLogWritten(p.AlphaLogPath()) {
		t.Errorf("zordon plan wrote %s; plan must not spawn alpha", p.AlphaLogPath())
	}
}

func TestPlan_subsetRendersOnlyPickedServicesPlusDeps(t *testing.T) {
	p := zordontest.NewProject(t)

	// Three services at the invocation level:
	//   - db   (no deps)
	//   - app  (after db.runtime ready → pulls db in transitively)
	//   - lone (unrelated)
	// `zordon plan app` must render db + app and OMIT lone — the exact
	// subset `zordon start app` would bring up.
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

service "go" "db" {
  package = "example.com/db@v0.0.0"

  vars = {
    port = net::pickport()
  }
}

service "go" "app" {
  package = "example.com/app@v0.0.0"

  runtime {
    after = [service.go.db.runtime.ready]
  }
}

service "go" "lone" {
  package = "example.com/lone@v0.0.0"
}
`)

	res := p.Zordon("plan", "app").Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon plan app: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	out := res.Stdout

	if !strings.Contains(out, `service "go" "app"`) {
		t.Errorf("picked service `app` missing from plan output\n%s", out)
	}
	if !strings.Contains(out, `service "go" "db"`) {
		t.Errorf("transitive dep `db` (app's runtime.after) must be pulled into the subset\n%s", out)
	}
	if strings.Contains(out, `service "go" "lone"`) {
		t.Errorf("unpicked service `lone` must NOT appear in a subset plan\n%s", out)
	}
}

func TestPlan_unknownPickIsError(t *testing.T) {
	p := zordontest.NewProject(t)

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

service "go" "app" {
  package = "example.com/app@v0.0.0"
}
`)

	res := p.Zordon("plan", "nope").Run(t)
	if res.ExitCode == 0 {
		t.Fatalf("zordon plan nope unexpectedly succeeded\nstdout: %s\nstderr: %s",
			res.Stdout, res.Stderr)
	}
	combined := res.Stdout + res.Stderr
	if !strings.Contains(combined, "nope") || !strings.Contains(combined, "available") {
		t.Errorf("error should name the unknown pick and list available services; got:\n%s", combined)
	}
}

func TestPlan_errorsOnUnresolvableReference(t *testing.T) {
	p := zordontest.NewProject(t)

	// `service.go.missing.vars.port` references a service that doesn't
	// exist in this Alphasfile (nor in any parent — there is none).
	// `plan`'s contract is "every value bakes down to a constant or we
	// error out" — a partial dump would defeat the preflight purpose.
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

service "go" "app" {
  package = "example.com/app@v0.0.0"

  vars = {
    bad = "127.0.0.1:${service.go.missing.vars.port}"
  }
}
`)

	res := p.Zordon("plan").Run(t)
	if res.ExitCode == 0 {
		t.Fatalf("zordon plan unexpectedly succeeded on unresolvable ref\nstdout: %s\nstderr: %s",
			res.Stdout, res.Stderr)
	}
	// Error message should mention the offending service so the user
	// can find the typo. Either stdout or stderr — zordon writes
	// errors via zlog to stderr but some level may bleed elsewhere.
	combined := res.Stdout + res.Stderr
	if !strings.Contains(combined, "missing") {
		t.Errorf("error message should mention the missing service name; got:\n%s", combined)
	}
}

func TestPlan_chainRendersRootFirstWithHeaders(t *testing.T) {
	// A federation chain: parent Alphasfile one dir up + child
	// Alphasfile in the project dir. `plan` walks root → leaf, emits
	// one `# === [<hash>] <path> ===` banner per level. The child's
	// `service.go.parent.vars.X` reference must resolve to the parent's
	// concrete value, proving the ParentContext accumulator is wired
	// through the chain walk (not duplicated per-command).
	p := zordontest.NewProject(t)

	// Parent lives one dir above the project root. NewProject already
	// gives us a temp tree; we write into its parent.
	p.WriteFile("../Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

service "go" "parent" {
  package = "example.com/parent@v0.0.0"

  vars = {
    port = net::pickport()
  }
}
`)

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

service "go" "child" {
  package = "example.com/child@v0.0.0"

  vars = {
    port            = net::pickport()
    parent_endpoint = "127.0.0.1:${service.go.parent.vars.port}"
  }
}
`)

	res := p.Zordon("plan").Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon plan: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	out := res.Stdout

	// Two `# === ... ===` banners — one per level.
	bannerRE := regexp.MustCompile(`(?m)^# === \[[0-9a-f]+\] .+ ===$`)
	if got := len(bannerRE.FindAllString(out, -1)); got != 2 {
		t.Errorf("expected 2 level banners, got %d\n%s", got, out)
	}
	// Invocation marker only on the leaf (child).
	if !strings.Contains(out, "(invocation) ===") {
		t.Errorf("expected `(invocation)` marker on leaf banner\n%s", out)
	}

	// Parent's port must appear BEFORE the child's section (root-first
	// order) AND be inlined into the child's parent_endpoint.
	parentIdx := strings.Index(out, `service "go" "parent"`)
	childIdx := strings.Index(out, `service "go" "child"`)
	if parentIdx < 0 || childIdx < 0 {
		t.Fatalf("missing parent or child service block\n%s", out)
	}
	if parentIdx >= childIdx {
		t.Errorf("parent service rendered after child; expected root-first order\n%s", out)
	}

	// Extract parent's port and verify the child's parent_endpoint
	// matches — pure proof that the federation accumulator works on
	// the static path, not just on the live-alpha path.
	parentBlock := out[parentIdx:childIdx]
	pp := regexp.MustCompile(`(?m)^\s*port\s*=\s*(\d+)\s*$`).FindStringSubmatch(parentBlock)
	if pp == nil {
		t.Fatalf("could not find parent's resolved port:\n%s", parentBlock)
	}
	wantEndpoint := `parent_endpoint = "127.0.0.1:` + pp[1] + `"`
	if !strings.Contains(out[childIdx:], wantEndpoint) {
		t.Errorf("expected %q in child block (cross-level ref not resolved)\n%s",
			wantEndpoint, out[childIdx:])
	}
}
