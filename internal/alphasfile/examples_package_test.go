package alphasfile

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// examples/package: the stack imports caddy with both features, which
// requires hugo and coredns; caddy on its own keeps its gated blocks off.
// Resolved through the real files, no process spawned.
func TestExamplePackageResolves(t *testing.T) {
	root, err := filepath.Abs("../../examples/package/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"caddy/caddy", "hugo/hugo", "coredns/coredns"}) {
		t.Fatalf("services = %v", got)
	}
	caddy := svcByName(af, "caddy/caddy")
	hugo := svcByName(af, "hugo/hugo")
	coredns := svcByName(af, "coredns/coredns")
	files := map[string]string{}
	for _, f := range caddy.Runtime.Files {
		files[f.Name] = f.Body
	}
	if body := files["site-hugo"]; !strings.Contains(body, fmt.Sprintf("127.0.0.1:%v", hugo.Runtime.Vars["port"])) {
		t.Errorf("site-hugo = %q, want hugo's port", body)
	}
	if body := files["site-coredns"]; !strings.Contains(body, "name health.test") || !strings.Contains(body, fmt.Sprintf("resolvers 127.0.0.1:%v", coredns.Runtime.Vars["dns"])) {
		t.Errorf("site-coredns = %q", body)
	}
}

func TestExamplePackageCaddyAlone(t *testing.T) {
	root, err := filepath.Abs("../../examples/package/caddy/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"caddy"}) {
		t.Fatalf("services = %v, want caddy alone: features are off", got)
	}
	got := fileNames(svcByName(af, "caddy"))
	sort.Strings(got)
	if !equalStrs(got, []string{"caddyfile", "site-base"}) {
		t.Errorf("files = %v", got)
	}
}

func TestExamplePackagePrintsWhereHugoIs(t *testing.T) {
	stack, err := filepath.Abs("../../examples/package/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	af := openTree(t, stack)
	caddy, hugo := svcByName(af, "caddy/caddy"), svcByName(af, "hugo/hugo")
	if want := fmt.Sprintf("http://127.0.0.1:%v/  (caddy, proxies to hugo)", caddy.Runtime.Vars["http"]); caddy.Runtime.Print != want {
		t.Errorf("caddy print = %q, want %q", caddy.Runtime.Print, want)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%v/  (hugo, direct)", hugo.Runtime.Vars["port"]); hugo.Runtime.Print != want {
		t.Errorf("hugo print = %q, want %q", hugo.Runtime.Print, want)
	}

	alone, err := filepath.Abs("../../examples/package/caddy/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	caddy = svcByName(openTree(t, alone), "caddy")
	if want := fmt.Sprintf("http://127.0.0.1:%v/  (caddy)", caddy.Runtime.Vars["http"]); caddy.Runtime.Print != want {
		t.Errorf("caddy alone print = %q, want %q", caddy.Runtime.Print, want)
	}
}
