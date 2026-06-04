package zordontest

import (
	"reflect"
	"testing"
)

// missingSubstrings is the pure core behind StartOutcome.OutputContains:
// it must report exactly the wants absent from the haystack, in order,
// and nothing when all are present. Tested directly so the assertion
// logic doesn't need a real (slow) zordon run to be trusted.
func TestMissingSubstrings(t *testing.T) {
	const out = "error: debugger.enabled = true is incompatible with runtime.cmd"
	cases := []struct {
		name  string
		wants []string
		miss  []string
	}{
		{"all present", []string{"debugger.enabled = true", "runtime.cmd"}, nil},
		{"one absent", []string{"debugger.enabled = true", "wrap_runtime = false"}, []string{"wrap_runtime = false"}},
		{"order preserved", []string{"zzz", "debugger", "aaa"}, []string{"zzz", "aaa"}},
		{"empty wants", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := missingSubstrings(out, c.wants); !reflect.DeepEqual(got, c.miss) {
				t.Errorf("missingSubstrings(...) = %q, want %q", got, c.miss)
			}
		})
	}
}
