package alphasfile

import (
	"strings"
	"testing"
)

func TestParseTree_rejectsModuleImport(t *testing.T) {
	_, err := ParseTree("test.hcl", []byte("component \"a\" {\n  import \"./Alphasfile.f\" { components = [\"m\"] }\n}\n"))
	if err == nil || !strings.Contains(err.Error(), "imports need a file on disk") {
		t.Fatalf("got %v", err)
	}
}
