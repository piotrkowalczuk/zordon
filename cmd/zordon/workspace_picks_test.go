package main

import (
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

func workspaceMetas() []*alphasfile.ServiceMeta {
	git := func(name, module string) *alphasfile.ServiceMeta {
		return &alphasfile.ServiceMeta{Name: name, Module: module, Package: &alphasfile.Package{Git: "github.com/x/" + name}}
	}
	return []*alphasfile.ServiceMeta{
		git("web", ""),
		git("auth/api", "auth"),
		{Name: "auth/db", Module: "auth", Package: &alphasfile.Package{}},
		{Name: "tools/tools/lint", Module: "tools/tools", Package: &alphasfile.Package{}},
	}
}

func TestWorkspaceTargets(t *testing.T) {
	got, err := workspaceTargets(workspaceMetas(), []string{"auth", "web@feat", "api"})
	if err != nil {
		t.Fatal(err)
	}
	want := []checkoutTarget{{svc: "auth/api"}, {svc: "web", rev: "feat"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("targets = %+v, want %+v; a module checks out its members with a source, once", got, want)
	}
}

func TestWorkspaceTargets_errors(t *testing.T) {
	cases := map[string]struct{ pick, want string }{
		"revision on a group":  {pick: "auth@feat", want: `"auth@feat": a revision names one service's checkout; pick the services of auth one by one`},
		"group with no source": {pick: "tools", want: `"tools" has no service with a git or dir source; nothing to check out`},
		"unknown":              {pick: "nope", want: `unknown service, module or package "nope" (available services: auth/api, auth/db, tools/tools/lint, web; modules: auth, tools/tools; packages: tools)`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			_, err := workspaceTargets(workspaceMetas(), []string{c.pick})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestWorkspaceTargets_singleServiceKeepsItsRevision(t *testing.T) {
	targets, err := workspaceTargets(workspaceMetas(), []string{"db@x"})
	if err != nil || len(targets) != 1 || targets[0] != (checkoutTarget{svc: "auth/db", rev: "x"}) {
		t.Fatalf("a single service keeps @rev; its source is checked when it is checked out: %+v, %v", targets, err)
	}
}
