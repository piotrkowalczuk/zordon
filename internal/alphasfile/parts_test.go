package alphasfile

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestOpen_partsOfTheEntrypointAddUp(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":    "sysenv = [\"HOME\"]\nenv = { A = \"1\" }\ncomponent \"api\" {\n  service \"go\" \"api\" {\n    git { url = \"github.com/x/api\" }\n    vars = { db = component.db.service.go.db.name }\n  }\n}\n",
		"Alphasfile.db": "sysenv = [\"PATH\", \"HOME\"]\nenv = { B = \"2\" }\ncomponent \"db\" {\n  service \"go\" \"db\" {\n    git { url = \"github.com/x/db\" }\n  }\n}\n",
	})
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"api/api", "db/db"}) {
		t.Errorf("services = %v; a part's components join the unit without an import", got)
	}
	if got := fmt.Sprint(svcByName(af, "api/api").Runtime.Vars["db"]); got != "db" {
		t.Errorf("db = %q; a component sees the components of another part", got)
	}
	if af.Env["A"] != "1" || af.Env["B"] != "2" {
		t.Errorf("env = %v; the env of every part adds up", af.Env)
	}
	if !equalStrs(af.SysEnv, []string{"HOME", "PATH"}) {
		t.Errorf("sysenv = %v; the sysenv of every part is a union", af.SysEnv)
	}
}

func TestOpen_partsOfAPackageAddUp(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile": `import "./web" { features = ["cache"] }`,
		"web/Alphasfile": `
package "web" {
  component "web" {
    service "go" "web" {
      git { url = "github.com/x/web" }
      vars = { cache = component.cache.service.go.redis.name }
    }
  }
}
`,
		"web/Alphasfile.cache": `
package "web" {
  features = { cache = { description = "Runs Redis." } }
  component "cache" {
    service "go" "redis" {
      enabled = features.cache
      git { url = "github.com/x/redis" }
    }
  }
}
`,
	})
	af := openTree(t, root)
	if got := serviceNames(af); !equalStrs(got, []string{"web/web/web", "web/cache/redis"}) {
		t.Errorf("services = %v; a feature declared in one part gates a block and is set by an importer", got)
	}
}

func TestLoadTree_partRules(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"package part without the package block": {
			files: map[string]string{"Alphasfile": `import "./p" {}`, "p/Alphasfile": `package "p" {}`, "p/Alphasfile.x": `component "x" {}`},
			want:  `a part of package "p"`,
		},
		"package part naming another package": {
			files: map[string]string{"Alphasfile": `import "./p" {}`, "p/Alphasfile": `package "p" {}`, "p/Alphasfile.x": `package "q" {}`},
			want:  `package "q", but`,
		},
		"package part with a block outside the package": {
			files: map[string]string{"Alphasfile": `import "./p" {}`, "p/Alphasfile": `package "p" {}`, "p/Alphasfile.x": "package \"p\" {}\ncomponent \"x\" {}\n"},
			want:  `component "x" outside package "p"`,
		},
		"entrypoint part with a package block": {
			files: map[string]string{"Alphasfile": `component "a" {}`, "Alphasfile.x": `package "p" {}`},
			want:  `package "p" in`,
		},
		"importing a file": {
			files: map[string]string{"Alphasfile": `import "./lib/Alphasfile.x" {}`, "lib/Alphasfile.x": `component "x" {}`},
			want:  `names a file; an import names a package's directory`,
		},
		"a directory with parts only": {
			files: map[string]string{"Alphasfile": `import "./lib" {}`, "lib/Alphasfile.x": `package "lib" {}`},
			want:  "has no Alphasfile, so it is not a package",
		},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), c.files)
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_partCollisions(t *testing.T) {
	pkg := func(body string) map[string]string {
		return map[string]string{
			"Alphasfile":     `import "./p" {}`,
			"p/Alphasfile":   "package \"p\" {\n" + body + "\n}\n",
			"p/Alphasfile.x": "package \"p\" {\n" + body + "\n}\n",
		}
	}
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"component":      {files: map[string]string{"Alphasfile": `component "a" {}`, "Alphasfile.x": `component "a" {}`}, want: `duplicate component "a"`},
		"feature":        {files: pkg(`features = { f = { description = "F." } }`), want: `feature "f" of package "p" is already declared in another part`},
		"input":          {files: pkg(`inputs = { i = { description = "I.", type = string, default = "x" } }`), want: `input "i" of package "p" is already declared in another part`},
		"output":         {files: pkg(`outputs = { o = { description = "O.", type = string, value = "x" } }`), want: `output "o" of package "p" is already declared in another part`},
		"toolchain lang": {files: map[string]string{"Alphasfile": "toolchain {\n  go { version = \"1.27.0\" }\n}\n", "Alphasfile.x": "toolchain {\n  go { version = \"1.22.0\" }\n}\n"}, want: "toolchain go is already pinned at"},
		"workspace":      {files: map[string]string{"Alphasfile": "workspace {\n  branch = \"a/${service.name}\"\n}\n", "Alphasfile.x": "workspace {\n  branch = \"b/${service.name}\"\n}\n"}, want: "workspace is already declared at"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), c.files)
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestOpen_partsJoinToolchainsOfDifferentLanguages(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   "toolchain {\n  go { version = \"1.27.0\" }\n}\n",
		"Alphasfile.x": "toolchain {\n  ruby { version = \"3.3.5\" }\n}\n",
	})
	af := openTree(t, root)
	if af.Toolchain["go"] == nil || af.Toolchain["ruby"] == nil {
		t.Errorf("toolchain = %v; two parts may pin two languages", toolchainKeys(af))
	}
}

func TestOpen_envKeyInTwoParts(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   `env = { A = "1" }`,
		"Alphasfile.x": `env = { A = "2" }`,
	})
	_, err := Open(root, testInv(), nil, testCfgHash, TestConfig{})
	if err == nil || !strings.Contains(err.Error(), `env "A" is already set at`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_bytesChangeWhenAPartOfAPackageChanges(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":     `import "./p" {}`,
		"p/Alphasfile":   `package "p" {}`,
		"p/Alphasfile.x": `package "p" {}`,
	})
	before, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"p/Alphasfile.x": "package \"p\" {\n  component \"x\" {}\n}\n"})
	after, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Bytes()) == string(after.Bytes()) {
		t.Error("editing a part of a package must change the manifest hash")
	}
}

func TestRenderWorkspace_blockInAPart(t *testing.T) {
	spec := mustRenderWorkspace(t, `component "a" {}`, map[string]string{"Alphasfile.ws": "workspace {\n  branch = \"team/${service.name}\"\n}\n"})
	if got := spec.BranchTemplate(); got != "team/${service.name}" {
		t.Errorf("branch template = %q; the workspace block may live in a part", got)
	}
}

func TestLoadTreeWith_symlinkedPartCannotLeaveTheCheckout(t *testing.T) {
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"Alphasfile.evil": `package "web" {}`})
	infra := gitRepo(t, map[string]string{"pkgs/web/Alphasfile": pkgWeb})
	if err := zfs.Symlink(filepath.Join(outside, "Alphasfile.evil"), filepath.Join(infra, "pkgs", "web", "Alphasfile.evil")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, infra, "add", "-A")
	gitRun(t, infra, "-c", "user.email=a@b", "-c", "user.name=z", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "link")
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": webAt("main")})
	_, err := LoadTreeWith(root, remoteOpts(t, dir, newRepoFetcher(map[string]string{remoteRepo: infra})))
	if err == nil || !strings.Contains(err.Error(), "leaves the checkout of github.com/acme/infra@") {
		t.Fatalf("a part of a fetched package that links out of the checkout must not be read, got %v", err)
	}
}

func TestParseServices_readsParts(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   "service \"go\" \"gw\" {\n  git { url = \"github.com/x/gw\" }\n}\n",
		"Alphasfile.x": "component \"x\" {\n  service \"go\" \"api\" {\n    git { url = \"github.com/x/api\" }\n  }\n}\n",
	})
	metas, err := ParseServices(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range metas {
		names = append(names, m.Name)
	}
	if !slices.Contains(names, "x/api") {
		t.Errorf("metas = %v; zordon workspace sees the services of every part", names)
	}
}
