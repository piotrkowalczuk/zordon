package main

import (
	"strings"
	"testing"
)

// stackPickables is a top-level service, a module of two services, and two
// packages that each have a service named worker; caddy is a package whose
// module and service share its name.
func stackPickables() []pickable {
	return []pickable{
		{name: "web", svc: "web"},
		{name: "api/http", module: "api", svc: "http"},
		{name: "api/grpc", module: "api", svc: "grpc"},
		{name: "shop/shop/worker", module: "shop/shop", svc: "worker"},
		{name: "shop/admin/panel", module: "shop/admin", svc: "panel"},
		{name: "billing/billing/worker", module: "billing/billing", svc: "worker"},
		{name: "caddy/caddy/caddy", module: "caddy/caddy", svc: "caddy"},
	}
}

func TestExpandPicks(t *testing.T) {
	cases := map[string]struct {
		picks []string
		want  []string
	}{
		"top-level service":                  {picks: []string{"web"}, want: []string{"web"}},
		"unique service in a module":         {picks: []string{"grpc"}, want: []string{"api/grpc"}},
		"unique service in a package":        {picks: []string{"panel"}, want: []string{"shop/admin/panel"}},
		"display name":                       {picks: []string{"shop/shop/worker"}, want: []string{"shop/shop/worker"}},
		"short name":                         {picks: []string{"billing/worker"}, want: []string{"billing/billing/worker"}},
		"module":                             {picks: []string{"api"}, want: []string{"api/http", "api/grpc"}},
		"package":                            {picks: []string{"shop"}, want: []string{"shop/shop/worker", "shop/admin/panel"}},
		"module of a package":                {picks: []string{"shop/admin"}, want: []string{"shop/admin/panel"}},
		"one service, three spellings agree": {picks: []string{"caddy"}, want: []string{"caddy/caddy/caddy"}},
		"deduplicated, stack order":          {picks: []string{"shop", "web", "panel"}, want: []string{"web", "shop/shop/worker", "shop/admin/panel"}},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := expandPicks(stackPickables(), c.picks)
			if err != nil {
				t.Fatal(err)
			}
			if !equalStrings(got, c.want) {
				t.Errorf("expandPicks(%v) = %v, want %v", c.picks, got, c.want)
			}
		})
	}
}

func TestExpandPicks_errors(t *testing.T) {
	cases := map[string]struct {
		picks []string
		want  []string
	}{
		"service name twice":         {picks: []string{"worker"}, want: []string{`"worker" names more than one thing`, "service shop/shop/worker", "service billing/billing/worker"}},
		"unknown after a valid pick": {picks: []string{"shop/worker", "x"}, want: []string{"unknown service(s), module(s) or package(s): x"}},
		"unknown":                    {picks: []string{"nope", "zz"}, want: []string{"nope, zz", "available services: ", "modules: api, billing/billing", "packages: billing, caddy, shop"}},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			_, err := expandPicks(stackPickables(), c.picks)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestResolvePick_differentSelectionsComplain(t *testing.T) {
	all := []pickable{
		{name: "web/web/web", module: "web/web", svc: "web"},
		{name: "web/web/jobs", module: "web/web", svc: "jobs"},
	}
	_, err := resolvePick(all, "web")
	if err == nil || !strings.Contains(err.Error(), "service web/web/web: web/web/web") || !strings.Contains(err.Error(), "package web: web/web/web, web/web/jobs") {
		t.Fatalf("a package and a service sharing a name but selecting different services must complain, got %v", err)
	}
	sel, err := resolvePick(all, "web/web/web")
	if err != nil || !sel.single {
		t.Errorf("a full name picks one service: %+v, %v", sel, err)
	}
	if sel, err := resolvePick(all, "jobs"); err != nil || !sel.single {
		t.Errorf("a unique bare name picks one service: %+v, %v", sel, err)
	}
}

func TestResolvePick_moduleAndPackageShareAName(t *testing.T) {
	all := []pickable{
		{name: "web/api", module: "web", svc: "api"},
		{name: "web/web/site", module: "web/web", svc: "site"},
	}
	_, err := resolvePick(all, "web")
	if err == nil || !strings.Contains(err.Error(), "module web: web/api") || !strings.Contains(err.Error(), "package web: web/web/site") {
		t.Fatalf("got %v", err)
	}
	if sel, err := resolvePick(all, "web/web"); err != nil || sel.single || !equalStrings(sel.names, []string{"web/web/site"}) {
		t.Errorf("the module of the package still has its own spelling: %+v, %v", sel, err)
	}
}
