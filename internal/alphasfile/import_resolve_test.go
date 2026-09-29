package alphasfile

import (
	"testing"
)

func TestOpen_moduleToolchainFromFragment(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": "toolchain {\n  go { version = \"1.27.0\" }\n}\n",
		"Alphasfile.legacy": `
component "legacy" {
  toolchain {
    go { version = "1.22.0" }
  }
  service "go" "billing" {
    git { url = "github.com/x/billing" }
  }
}
`,
	})
	af := openTree(t, root)
	if af.Toolchain["legacy/go"] == nil || af.Toolchain["legacy/go"].Version != "1.22.0" {
		t.Fatalf("toolchain = %v", toolchainKeys(af))
	}
	if got := svcByName(af, "legacy/billing").ToolchainKey; got != "legacy/go" {
		t.Errorf("ToolchainKey = %q", got)
	}
}

func TestParseServices_followsImports(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile": "service \"go\" \"gw\" {\n  src { path = \".\" }\n}\n",
		"Alphasfile.svc": `
component "svc" {
  service "go" "api" {
    src { path = "." }
  }
}
`,
	})
	metas, err := ParseServices(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 || metas[1].Name != "svc/api" || metas[1].Package.Src != dir {
		t.Errorf("metas = %+v / %+v", metas, metas[len(metas)-1].Package)
	}
}

func openTree(t *testing.T, root string) *Alphasfile {
	t.Helper()
	af, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return af
}
