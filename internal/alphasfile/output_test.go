package alphasfile

import (
	"fmt"
	"strings"
	"testing"
)

const pkgDatabase = `
package "db" {
  slots = {
    databases = {
      description = "Databases to create."
      entry       = object({ name = string })
      unique      = ["name"]
    }
  }

  outputs = {
    port  = module.db.service.go.pg.vars.port
    ready = module.db.service.go.pg.runtime.ready
    dsn   = { for key, d in slots.databases : key => "postgres://127.0.0.1:${module.db.service.go.pg.vars.port}/${d.name}" }
  }

  module "db" {
    service "go" "pg" {
      git { url = "github.com/x/pg" }
      vars = { port = 5432 }
    }
  }
}
`

const pkgOrders = `
package "orders" {
  import "../db" {
    provide "databases" {
      name = "orders"
    }
  }

  module "orders" {
    service "go" "orders" {
      git { url = "github.com/x/orders" }
      vars = { dsn = package.db.outputs.dsn.orders, port = package.db.outputs.port }
      runtime {
        after = [package.db.outputs.ready]
      }
    }
  }
}
`

func TestOpen_outputsReachTheImporter(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":        `import "./orders" {}`,
		"db/Alphasfile":     pkgDatabase,
		"orders/Alphasfile": pkgOrders,
	})
	orders := svcByName(openTree(t, root), "orders/orders/orders")
	if got := fmt.Sprint(orders.Runtime.Vars["dsn"], " ", orders.Runtime.Vars["port"]); got != "postgres://127.0.0.1:5432/orders 5432" {
		t.Errorf("vars = %s; an output may read services and slots, and an entry reads its own value back by its key", got)
	}
	if after := orders.Runtime.After; len(after) != 1 || !strings.HasSuffix(after[0], "package.db.module.db.service.go.pg.runtime@ready") {
		t.Errorf("after = %v; an output carries a barrier like the reference it names", after)
	}
}

func TestOpen_entrypointReadsAnOutput(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":    "import \"./db\" {}\n\nservice \"go\" \"tool\" {\n  git { url = \"github.com/x/tool\" }\n  vars = { db = package.db.outputs.port }\n}\n",
		"db/Alphasfile": pkgDatabase,
	})
	if got := fmt.Sprint(svcByName(openTree(t, root), "tool").Runtime.Vars["db"]); got != "5432" {
		t.Errorf("db = %s", got)
	}
}

func TestOpen_outputErrors(t *testing.T) {
	cases := map[string]struct{ ref, want string }{
		"unknown output": {"package.db.outputs.nope", `package db has no output "nope" (outputs: dsn, port, ready)`},
		"bare outputs":   {"package.db.outputs", "package.db.outputs: name an output"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":    "import \"./db\" {}\n\nservice \"go\" \"tool\" {\n  git { url = \"github.com/x/tool\" }\n  vars = { db = " + c.ref + " }\n}\n",
				"db/Alphasfile": pkgDatabase,
			})
			_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestOpen_outputOfAPackageNotImported(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":        "import \"./db\" {}\nimport \"./orders\" {}\n",
		"db/Alphasfile":     pkgDatabase,
		"orders/Alphasfile": strings.Replace(pkgOrders, "  import \"../db\" {\n    provide \"databases\" {\n      name = \"orders\"\n    }\n  }\n", "", 1),
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `package.db is not visible in package "orders"`) {
		t.Fatalf("an output is read through an import like any other symbol, got %v", err)
	}
}

func TestLoadTree_outputDeclarationErrors(t *testing.T) {
	cases := map[string]struct{ outputs, want string }{
		"not an object": {`["port"]`, `outputs of package "p" map each name to a value`},
		"bad name":      {`{ "no spaces" = 1 }`, `output of package "p": name it with letters`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":   `import "./p" {}`,
				"p/Alphasfile": "package \"p\" {\n  outputs = " + c.outputs + "\n}\n",
			})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestOpen_outputEvaluationError(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   "import \"./p\" {}\n\nservice \"go\" \"tool\" {\n  git { url = \"github.com/x/tool\" }\n  vars = { v = package.p.outputs.broken }\n}\n",
		"p/Alphasfile": "package \"p\" {\n  outputs = { broken = module.m.service.go.s.vars.nope }\n  module \"m\" {\n    service \"go\" \"s\" {\n      git { url = \"github.com/x/s\" }\n    }\n  }\n}\n",
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `output "broken" of package p`) {
		t.Fatalf("got %v", err)
	}
}
