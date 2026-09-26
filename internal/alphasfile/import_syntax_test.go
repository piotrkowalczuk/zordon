package alphasfile

import (
	"strings"
	"testing"
)

func TestLoadTree_importPathNeedsLocalPrefix(t *testing.T) {
	cases := map[string]struct {
		path string
		ok   bool
		want string
	}{
		"dot slash":      {path: "./Alphasfile.f", ok: true},
		"parent":         {path: "../x/Alphasfile.f", ok: true},
		"bare relative":  {path: "Alphasfile.f", want: "is neither a local path (those start with ./, ../, / or ~/) nor a remote identity"},
		"remote looking": {path: "github.com/acme/infra/Alphasfile.f", want: "a remote import needs a version"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			dir := t.TempDir()
			root := writeTree(t, dir+"/x", map[string]string{
				"Alphasfile":   `import "` + c.path + `" { modules = ["m"] }`,
				"Alphasfile.f": `module "m" {}`,
			})
			_, err := LoadTree(root)
			if c.ok {
				if err != nil && (strings.Contains(err.Error(), "remote") || strings.Contains(err.Error(), "neither a local path")) {
					t.Fatalf("a local path was treated as remote: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_importInsideModuleIsRejected(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   "module \"a\" {\n  import \"./Alphasfile.f\" { modules = [\"m\"] }\n}\n",
		"Alphasfile.f": `module "m" {}`,
	})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), `Blocks of type "import" are not expected here`) {
		t.Fatalf("got %v", err)
	}
}
