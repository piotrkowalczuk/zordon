// Import conformance, driven through `zordon plan` (static, no alpha):
// fragments load and render under their level, an edit to a fragment changes
// the manifest hash that federation drift compares, a fragment's directory
// resolves to the entrypoint's instance, and the load-time errors reach the
// user with the file that caused them.
package conformance_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

const importEntry = `
sysenv = ["HOME", "USER", "PATH", "TMPDIR"]

import "services/apps/Alphasfile.apps" {
  modules = ["app"]
}
`

const importApps = `
module "app" {
  import "../db/Alphasfile.db" {
    modules = ["db"]
  }

  service "go" "app" {
    package = "example.com/app@v0.0.0"
    vars = {
      db  = "127.0.0.1:${module.db.service.go.db.vars.port}"
      cfg = cfg::hash()
    }
  }
}
`

const importDB = `
module "db" {
  service "go" "db" {
    package = "example.com/db@v0.0.0"
    vars = { port = net::pickport() }
  }
}
`

func TestPlan_importedModulesRenderUnderLevel(t *testing.T) {
	p := newImportProject(t)
	res := p.Zordon("plan").Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon plan: exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	out := res.Stdout
	dir, err := filepath.EvalSymlinks(p.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# import " + filepath.Join(dir, "services/apps/Alphasfile.apps") + " [app]",
		"# import " + filepath.Join(dir, "services/db/Alphasfile.db") + " [db]",
		`module "app" {`,
		`module "db" {`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	port := regexp.MustCompile(`(?m)^\s*port\s*=\s*(\d+)\s*$`).FindStringSubmatch(out)
	if port == nil || !strings.Contains(out, `db  = "127.0.0.1:`+port[1]+`"`) {
		t.Errorf("app's db address must carry db's concrete port\n%s", out)
	}
}

func TestPlan_fragmentEditChangesHash(t *testing.T) {
	p := newImportProject(t)
	before := planCfgHash(t, p)
	p.WriteFile("services/db/Alphasfile.db", "sysenv = [\"TZ\"]\n"+importDB)
	after := planCfgHash(t, p)
	if before == after {
		t.Fatalf("editing an imported fragment must change cfg::hash() (both %s)", before)
	}
}

func TestPlan_fragmentDirResolvesToEntrypoint(t *testing.T) {
	p := newImportProject(t)
	root := planHeader(t, p.Zordon("plan").Run(t).Stdout)
	sub := planHeader(t, p.Zordon("plan").WithDir("services/db").Run(t).Stdout)
	if root == "" || root != sub {
		t.Fatalf("a fragment's dir must resolve to the entrypoint's instance: root %q, fragment dir %q", root, sub)
	}
}

func TestPlan_unimportedModuleRefFails(t *testing.T) {
	p := newImportProject(t)
	p.WriteFile("Alphasfile", importEntry+`
service "go" "gw" {
  package = "example.com/gw@v0.0.0"
  vars = { db = module.db.service.go.db.vars.port }
}
`)
	res := p.Zordon("plan").Run(t)
	if res.ExitCode == 0 {
		t.Fatalf("the entrypoint does not import module db; plan must fail\n%s", res.Stdout)
	}
	for _, want := range []string{"module.db is not visible", `add import "services/db/Alphasfile.db" { modules = ["db"] }`} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("missing %q in stderr:\n%s", want, res.Stderr)
		}
	}
}

func TestPlan_importOfEntrypointFails(t *testing.T) {
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", `import "svc/Alphasfile" { modules = ["svc"] }`)
	p.WriteFile("svc/Alphasfile", `module "svc" {}`)
	res := p.Zordon("plan").Run(t)
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "entrypoints and form federation levels") {
		t.Fatalf("exit %d, stderr:\n%s", res.ExitCode, res.Stderr)
	}
}

func newImportProject(t *testing.T) *zordontest.Project {
	t.Helper()
	p := zordontest.NewProject(t)
	p.WriteFile("Alphasfile", importEntry)
	p.WriteFile("services/apps/Alphasfile.apps", importApps)
	p.WriteFile("services/db/Alphasfile.db", importDB)
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

func planHeader(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^# === \[([0-9a-f]+)\] (\S+)`).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1] + " " + m[2]
}
