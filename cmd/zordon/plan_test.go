package main

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

func TestRenderSource_pathBesideGit(t *testing.T) {
	cases := map[string]struct {
		pkg alphasfile.Package
	}{
		"present": {alphasfile.Package{Toolchain: "go", Git: "github.com/acme/ui", Src: "/checkouts/ui", Exe: "./ui"}},
		"missing": {alphasfile.Package{Toolchain: "go", Git: "github.com/acme/ui", SrcMissing: "/checkouts/ui", Exe: "./ui"}},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			f := hclwrite.NewEmptyFile()
			renderSource(f.Body(), &c.pkg)
			got := strings.Join(strings.Fields(string(f.Bytes())), " ")
			for _, want := range []string{`url = "github.com/acme/ui"`, `path = "/checkouts/ui"`, `exe = "./ui"`} {
				if !strings.Contains(got, want) {
					t.Errorf("renderSource() = %q, want %q in it", got, want)
				}
			}
		})
	}
}
