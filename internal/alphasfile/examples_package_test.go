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
	if body := files["site-hugo-host"]; !strings.Contains(body, "@hugo host hugo.test") || !strings.Contains(body, fmt.Sprintf("127.0.0.1:%v", hugo.Runtime.Vars["port"])) {
		t.Errorf("site-hugo-host = %q, want the hugo.test host routed to hugo's port", body)
	}
	if _, pathRoute := files["site-hugo"]; pathRoute {
		t.Error("with DNS, hugo is routed by host, not by path")
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
	if want := fmt.Sprintf("http://hugo.test:%v/  (caddy routes the host to hugo)", caddy.Runtime.Vars["http"]); caddy.Runtime.Print != want {
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

func TestExamplePackageHugoWithoutDNS(t *testing.T) {
	caddyDir, err := filepath.Abs("../../examples/package/caddy")
	if err != nil {
		t.Fatal(err)
	}
	root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": `import "` + caddyDir + `" { features = ["hugo"] }`})
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"caddy/caddy", "hugo/hugo"}) {
		t.Fatalf("services = %v", got)
	}
	caddy := svcByName(af, "caddy/caddy")
	got := fileNames(caddy)
	sort.Strings(got)
	if !equalStrs(got, []string{"caddyfile", "site-base", "site-hugo"}) {
		t.Errorf("files = %v, want the path route and no host route without DNS", got)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%v/  (caddy, proxies to hugo)", caddy.Runtime.Vars["http"]); caddy.Runtime.Print != want {
		t.Errorf("print = %q, want %q", caddy.Runtime.Print, want)
	}
}

func TestExamplePackageResolverAndPort(t *testing.T) {
	caddyDir, err := filepath.Abs("../../examples/package/caddy")
	if err != nil {
		t.Fatal(err)
	}
	corednsDir := filepath.Join(filepath.Dir(caddyDir), "coredns")
	root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": `
import "` + caddyDir + `" {
  features = ["hugo", "coredns"]
  inputs   = { http_port = 80 }
}
import "` + corednsDir + `" {
  features = ["resolver"]
}
`})
	af := openTree(t, root)
	if got := fmt.Sprint(svcByName(af, "caddy/caddy").Runtime.Vars["http"]); got != "80" {
		t.Errorf("caddy http = %s, want the http_port input", got)
	}
	sudo := svcByName(af, "coredns/coredns").Runtime.Sudo
	if len(sudo) != 1 || sudo[0].Name != "resolver" || !strings.Contains(sudo[0].Apply, "/etc/resolver/test") || !strings.Contains(sudo[0].Apply, "port 49564") {
		t.Fatalf("sudo = %+v, want the resolver step for zone test on port 49564", sudo)
	}
}

func TestExamplePackageResolverIsOffByDefault(t *testing.T) {
	stack, err := filepath.Abs("../../examples/package/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	if sudo := svcByName(openTree(t, stack), "coredns/coredns").Runtime.Sudo; len(sudo) != 0 {
		t.Errorf("sudo = %+v, want none: the resolver needs root and stays off unless asked for", sudo)
	}
}
