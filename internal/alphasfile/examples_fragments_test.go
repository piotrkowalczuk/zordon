package alphasfile

import (
	"slices"
	"strings"
	"testing"
)

// examples/fragments: a package and its subpackage, each split into an
// Alphasfile and one part, and a stray part no Alphasfile anchors.
func TestExampleFragmentsResolves(t *testing.T) {
	af := openTree(t, "../../examples/fragments/Alphasfile")
	want := []string{"platform/api/api", "platform/jobs/worker", "ingest/collector/collector", "ingest/store/store"}
	if got := serviceNames(af); !equalStrs(got, want) {
		t.Fatalf("services = %v, want %v; tools/Alphasfile.lint has no Alphasfile and stays out", got, want)
	}
	upstream := func(name string) string {
		cmd := svcByName(af, name).Runtime.Command
		return cmd[slices.Index(cmd, "-upstream")+1]
	}
	port := func(name string) string {
		for _, a := range svcByName(af, name).Runtime.Command {
			if p, ok := strings.CutPrefix(a, "127.0.0.1:"); ok {
				return p
			}
		}
		t.Fatalf("%s has no address", name)
		return ""
	}
	cases := map[string]string{
		"platform/api/api":           "http://127.0.0.1:" + port("ingest/collector/collector"),
		"platform/jobs/worker":       "http://127.0.0.1:" + port("platform/api/api"),
		"ingest/collector/collector": "http://127.0.0.1:" + port("ingest/store/store"),
		"ingest/store/store":         "72h",
	}
	for name, want := range cases {
		if got := upstream(name); got != want {
			t.Errorf("%s upstream = %q, want %q", name, got, want)
		}
	}
}
