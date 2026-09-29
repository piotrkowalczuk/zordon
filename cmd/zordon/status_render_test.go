package main

import (
	"path/filepath"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

func TestStatusImports_local(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"Alphasfile":       "import \"./shop\" {}\nimport \"./caddy\" { features = [\"dns\"] }\n",
		"shop/Alphasfile":  "package \"shop\" {\n  import \"../caddy\" {}\n}\n",
		"caddy/Alphasfile": "package \"caddy\" {\n  features = { dns = { description = \"DNS.\" } }\n}\n",
	})
	tree, err := alphasfile.LoadTree(filepath.Join(dir, "Alphasfile"))
	if err != nil {
		t.Fatal(err)
	}
	want := "imports\n" +
		"  shop   ./shop\n" +
		"  caddy  ./caddy  ← shop   features: dns\n"
	if got := statusImports(tree, dir); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestStatusImports_remote(t *testing.T) {
	checkout, dir := t.TempDir(), t.TempDir()
	writeFiles(t, checkout, map[string]string{
		alphasfile.ModFileName: `module = "github.com/acme/infra"`,
		"pkgs/web/Alphasfile":  "package \"web\" {\n  import \"../db\" {}\n}\n",
		"pkgs/db/Alphasfile":   "package \"db\" {\n}\n",
	})
	writeFiles(t, dir, map[string]string{
		"Alphasfile": "require \"github.com/acme/infra\" { ref = \"main\" }\nimport \"github.com/acme/infra/pkgs/web\" {}\n",
	})
	tree, err := alphasfile.LoadTreeWith(filepath.Join(dir, "Alphasfile"), alphasfile.LoadOptions{Search: []string{checkout}})
	if err != nil {
		t.Fatal(err)
	}
	want := "imports\n" +
		"  web  github.com/acme/infra/pkgs/web  search " + checkout + "\n" +
		"  db   …/pkgs/db                       ← web\n"
	if got := statusImports(tree, dir); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestStatusServices_live(t *testing.T) {
	st := &protocol.StateInfo{PID: 9, Services: []*alphasfile.Service{
		statusSvc("shop/shop", "hugo", "http://127.0.0.1:1/"),
		statusSvc("caddy/caddy", "caddy", "http://127.0.0.1:2/"),
		statusSvc("", "tool", ""),
	}}
	want := "services\n" +
		"  shop/hugo — stopped     http://127.0.0.1:1/\n" +
		"  caddy/caddy — stopped   http://127.0.0.1:2/\n" +
		"  tool — stopped\n"
	if got := statusServices(t.Context(), st, "main"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestStatusServices_notRunningHidesPrint(t *testing.T) {
	st := &protocol.StateInfo{Services: []*alphasfile.Service{statusSvc("shop/shop", "hugo", "http://127.0.0.1:1/")}}
	if got, want := statusServices(t.Context(), st, "main"), "services\n  shop/hugo — stopped\n"; got != want {
		t.Errorf("got %q, want %q; ports picked for the report are not the service's", got, want)
	}
	if got, want := statusServices(t.Context(), &protocol.StateInfo{PID: 9}, "main"), "services: none configured yet\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStatusHeader(t *testing.T) {
	inv := &invocation.InvocationState{Workspace: "main", FsHash: "abc"}
	cases := map[string]struct {
		lv   *level
		want string
	}{
		"running":     {&level{afPath: "/w/Alphasfile", isInvocation: true, inv: inv, state: &protocol.StateInfo{PID: 7, StartedAt: "12:00"}}, "./Alphasfile  workspace main  [abc]  alpha: running pid=7 since 12:00\n"},
		"not running": {&level{afPath: "/w/Alphasfile", isInvocation: true, inv: inv, state: &protocol.StateInfo{}}, "./Alphasfile  workspace main  [abc]  alpha: not running\n"},
		"parent":      {&level{afPath: "/p/Alphasfile", inv: inv}, "/p/Alphasfile  [abc]  alpha: not running\n"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := statusHeader(c.lv, "/w"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func statusSvc(module, name, print string) *alphasfile.Service {
	return &alphasfile.Service{Toolchain: "go", Module: module, Runtime: &alphasfile.RuntimeConfig{Name: name, Print: print}}
}
