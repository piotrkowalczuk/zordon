package conformance_test

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// TestPkgToolchain_atlas_migratesSQLite brings up a standalone CLI declared
// under `toolchain { pkg { tools } }` — `atlas`, from mise's aqua backend —
// puts it on a provision's PATH via fs::toolchain::bin(toolchain.pkg), and runs
// `atlas migrate apply` against a local SQLite file. The service gates on the
// migration (runtime.after), so a green start proves the tool installed,
// resolved onto PATH, and executed. End-to-end proof of the pkg-tools path.
//
// atlas ships as a prebuilt single binary via aqua, so the install is a fast
// download (no source compile) — same cost profile as the etcd pkg test. The
// migration fixture is the one examples/pkg_tools ships, so its atlas.sum stays
// exercised.
func TestPkgToolchain_atlas_migratesSQLite(t *testing.T) {
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
  }

  runtime {
    after = [self.runtime.provision.migrate.success]

    provision "migrate" {
      env = { PATH = env::prepend(fs::toolchain::bin(toolchain.pkg)) }
      cmd = "atlas migrate apply --url sqlite://${self.vars.db}?_fk=1 --dir file://${fs::src()}/migrations"
    }

    cmd = ["${fs::bin()}/app", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    tcp { port = self.vars.port }
    period            = "200ms"
    failure_threshold = 100
  }
}
`)

	mustStart(t, p)

	// The app came up only because the migrate provision succeeded (runtime.after
	// gates on it). Confirm atlas actually applied the schema: SQLite stores the
	// CREATE TABLE text verbatim, so the `users` table name is present in the file.
	dbPath := p.Get(t, "service.go.app.vars.db").String()
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read migrated sqlite db %s: %v", dbPath, err)
	}
	if !strings.Contains(string(data), "users") {
		t.Fatalf("sqlite db present but 'users' table not found — atlas migration did not apply")
	}
}
