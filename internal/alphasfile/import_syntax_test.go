package alphasfile

import (
	"strings"
	"testing"
)

func TestLoadTree_importPathNeedsLocalPrefix(t *testing.T) {
	cases := map[string]struct {
		path string
		ok   bool
	}{
		"dot slash":      {"./Alphasfile.f", true},
		"parent":         {"../x/Alphasfile.f", true},
		"bare relative":  {"Alphasfile.f", false},
		"remote looking": {"github.com/acme/infra/Alphasfile.f", false},
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
				if err != nil && strings.Contains(err.Error(), "remote imports are not supported yet") {
					t.Fatalf("a local path was treated as remote: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "remote imports are not supported yet; a local path starts with ./, ../, / or ~/") {
				t.Fatalf("got %v", err)
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
