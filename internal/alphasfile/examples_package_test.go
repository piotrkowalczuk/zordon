package alphasfile

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// examples/package: shop and blog each provide a site to caddy, which
// routes them by host; the stack turns on caddy's dns feature, which imports
// coredns. Resolved through the real files, no process spawned.
func TestExamplePackageResolves(t *testing.T) {
	af := openTree(t, examplePackage(t, "Alphasfile"))
	if got := serviceNames(af); !equalStrs(got, []string{"shop/shop/hugo", "caddy/caddy/caddy", "coredns/coredns/coredns", "blog/blog/hugo"}) {
		t.Fatalf("services = %v", got)
	}
	caddy := svcByName(af, "caddy/caddy/caddy")
	coredns := svcByName(af, "coredns/coredns/coredns")
	routes := caddyFiles(caddy)["site-routes"]
	for _, site := range []string{"shop", "blog"} {
		port := svcByName(af, site+"/"+site+"/hugo").Runtime.Vars["port"]
		want := fmt.Sprintf("@%[1]s host %[1]s.test\nhandle @%[1]s {\n  reverse_proxy 127.0.0.1:%[2]v\n}\n", site, port)
		if !strings.Contains(routes, want) {
			t.Errorf("site-routes = %q, want the %s.test host routed to its port", routes, site)
		}
	}
	if body := caddyFiles(caddy)["site-dns"]; !strings.Contains(body, "name health.test") || !strings.Contains(body, fmt.Sprintf("resolvers 127.0.0.1:%v", coredns.Runtime.Vars["dns"])) {
		t.Errorf("site-dns = %q", body)
	}
}

func TestExamplePackageCaddyAlone(t *testing.T) {
	af := openTree(t, examplePackage(t, "caddy/Alphasfile"))
	if got := serviceNames(af); !equalStrs(got, []string{"caddy/caddy/caddy"}) {
		t.Fatalf("services = %v, want caddy alone: features are off and nobody provides a site", got)
	}
	caddy := svcByName(af, "caddy/caddy/caddy")
	got := fileNames(caddy)
	sort.Strings(got)
	if !equalStrs(got, []string{"caddyfile", "site-routes"}) {
		t.Errorf("files = %v", got)
	}
	if routes := caddyFiles(caddy)["site-routes"]; strings.Contains(routes, "host") {
		t.Errorf("site-routes = %q, want no host without a provided site", routes)
	}
}

func TestExamplePackageShopAlone(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": `import "` + filepath.Dir(examplePackage(t, "shop/Alphasfile")) + `" {}`})
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"shop/shop/hugo", "caddy/caddy/caddy"}) {
		t.Fatalf("services = %v, want the shop and the caddy it registers with", got)
	}
	routes := caddyFiles(svcByName(af, "caddy/caddy/caddy"))["site-routes"]
	if !strings.Contains(routes, "@shop host shop.test") || strings.Contains(routes, "blog") {
		t.Errorf("site-routes = %q, want the shop only", routes)
	}
}

func TestExamplePackagePrintsTheSites(t *testing.T) {
	af := openTree(t, examplePackage(t, "Alphasfile"))
	caddy := svcByName(af, "caddy/caddy/caddy")
	http := caddy.Runtime.Vars["http"]
	if want := fmt.Sprintf("http://127.0.0.1:%[1]v/  (caddy) http://blog.test:%[1]v/ http://shop.test:%[1]v/", http); caddy.Runtime.Print != want {
		t.Errorf("caddy print = %q, want %q", caddy.Runtime.Print, want)
	}
	shop := svcByName(af, "shop/shop/hugo")
	if want := fmt.Sprintf("http://127.0.0.1:%v/  (shop, direct)", shop.Runtime.Vars["port"]); shop.Runtime.Print != want {
		t.Errorf("shop print = %q, want %q", shop.Runtime.Print, want)
	}
}

func TestExamplePackageResolverAndPort(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": `
import "` + filepath.Dir(examplePackage(t, "Alphasfile")) + `" {}
import "` + filepath.Dir(examplePackage(t, "caddy/Alphasfile")) + `" {
  features = ["dns"]
  inputs   = { http_port = 80 }
}
import "` + filepath.Dir(examplePackage(t, "coredns/Alphasfile")) + `" {
  features = ["resolver"]
}
`})
	af := openTree(t, root)
	caddy := svcByName(af, "caddy/caddy/caddy")
	if got := fmt.Sprint(caddy.Runtime.Vars["http"]); got != "80" {
		t.Errorf("caddy http = %s, want the http_port input", got)
	}
	if !strings.Contains(caddy.Runtime.Print, "http://shop.test:80/") {
		t.Errorf("caddy print = %q", caddy.Runtime.Print)
	}
	sudo := svcByName(af, "coredns/coredns/coredns").Runtime.Sudo
	if len(sudo) != 1 || sudo[0].Name != "resolver" || !strings.Contains(sudo[0].Apply, "/etc/resolver/test") || !strings.Contains(sudo[0].Apply, "port 49564") {
		t.Fatalf("sudo = %+v, want the resolver step for zone test on port 49564", sudo)
	}
}

func TestExamplePackageEntrypointMustKeepTheStacksFeature(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": `
import "` + filepath.Dir(examplePackage(t, "Alphasfile")) + `" {}
import "` + filepath.Dir(examplePackage(t, "caddy/Alphasfile")) + `" {
  inputs = { http_port = 80 }
}
`})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), `package caddy runs with feature "dns" off, but package stack needs it`) {
		t.Fatalf("got %v", err)
	}
}

func TestExamplePackageResolverIsOffByDefault(t *testing.T) {
	if sudo := svcByName(openTree(t, examplePackage(t, "Alphasfile")), "coredns/coredns/coredns").Runtime.Sudo; len(sudo) != 0 {
		t.Errorf("sudo = %+v, want none: the resolver needs root and stays off unless asked for", sudo)
	}
}

func examplePackage(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("../../examples/package", rel))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func caddyFiles(s *Service) map[string]string {
	out := map[string]string{}
	for _, f := range s.Runtime.Files {
		out[f.Name] = f.Body
	}
	return out
}
