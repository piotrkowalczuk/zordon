// Parts conformance, driven through `zordon plan` (static, no alpha): every
// Alphasfile.<name> next to an Alphasfile renders under its level, an edit to
// a part changes the manifest hash that federation drift compares, and
// importing a file instead of a package's directory reaches the user as an
// error.
package conformance_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

const partsEntry = `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]
`

const partsApps = `
component "app" {
  service "go" "app" {
    package = "example.com/app@v0.0.0"
    vars = {
      db  = "127.0.0.1:${component.db.service.go.db.vars.port}"
      cfg = cfg::hash()
    }
  }
}
`

const partsDB = `
component "db" {
  service "go" "db" {
    package = "example.com/db@v0.0.0"
    vars = { port = net::pickport() }
  }
}
`

func TestPlan_partsRenderUnderLevel(t *testing.T) {
	p := newPartsProject(t)
	res := p.Zordon("plan").Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon plan: exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	out := res.Stdout
	for _, want := range []string{`component "app" {`, `component "db" {`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	port := regexp.MustCompile(`(?m)^\s*port\s*=\s*(\d+)\s*$`).FindStringSubmatch(out)
	if port == nil || !strings.Contains(out, `db  = "127.0.0.1:`+port[1]+`"`) {
		t.Errorf("app's db address must carry db's concrete port\n%s", out)
	}
}

func TestPlan_partEditChangesHash(t *testing.T) {
	p := newPartsProject(t)
	before := planCfgHash(t, p)
	p.WriteFile("Alphasfile.db", "# edited\n"+partsDB)
	after := planCfgHash(t, p)
	if before == after {
		t.Fatalf("editing a part of the Alphasfile must change cfg::hash() (both %s)", before)
	}
}

func TestPlan_importOfAFileFails(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", `import "./svc/Alphasfile" {}`)
	p.WriteFile("svc/Alphasfile", `component "svc" {}`)
	res := p.Zordon("plan").Run(t)
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "names a file; an import names a package's directory") {
		t.Fatalf("exit %d, stderr:\n%s", res.ExitCode, res.Stderr)
	}
}

func newPartsProject(t *testing.T) *zordontest.Project {
	t.Helper()
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", partsEntry)
	p.WriteFile("Alphasfile.apps", partsApps)
	p.WriteFile("Alphasfile.db", partsDB)
	return p
}

func planCfgHash(t *testing.T, p *zordontest.Project) string {
	t.Helper()
	res := p.Zordon("plan").Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon plan: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	m := regexp.MustCompile(`(?m)^\s*cfg\s*=\s*"([0-9a-f]+)"`).FindStringSubmatch(res.Stdout)
	if m == nil {
		t.Fatalf("no cfg = \"<hash>\" line in\n%s", res.Stdout)
	}
	return m[1]
}
