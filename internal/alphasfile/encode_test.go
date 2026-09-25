package alphasfile

import (
	"strings"
	"testing"
)

func TestEncodeFuncs(t *testing.T) {
	cases := map[string]struct {
		call string
		want string
	}{
		"json": {
			call: `enc::json({ b = 2, a = "x", ok = true, list = [1, 2] })`,
			want: "{\n  \"a\": \"x\",\n  \"b\": 2,\n  \"list\": [\n    1,\n    2\n  ],\n  \"ok\": true\n}\n",
		},
		"yaml": {
			call: `enc::yaml({ b = 2, a = "x", ok = true, list = [1, 2] })`,
			want: "a: x\nb: 2\nlist:\n  - 1\n  - 2\nok: true\n",
		},
		"toml": {
			call: `enc::toml({ b = 2, a = "x", ok = true, list = [1, 2] })`,
			want: "a = \"x\"\nb = 2\nlist = [1, 2]\nok = true\n",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := encodeInFileBody(t, c.call); got != c.want {
				t.Errorf("%s\ngot:\n%q\nwant:\n%q", c.call, got, c.want)
			}
		})
	}
}

// TestEncodeFuncs_integralNumbers pins the reason ctyToGo carries its own
// number branch: a port emitted as 8080.0 is rejected by most consumers.
func TestEncodeFuncs_integralNumbers(t *testing.T) {
	cases := map[string]struct {
		call string
		want string
	}{
		"json": {call: `enc::json({ port = 8080 })`, want: "8080"},
		"yaml": {call: `enc::yaml({ port = 8080 })`, want: "8080"},
		"toml": {call: `enc::toml({ port = 8080 })`, want: "8080"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got := encodeInFileBody(t, c.call)
			if !strings.Contains(got, c.want) {
				t.Errorf("%s = %q, want it to contain %q", c.call, got, c.want)
			}
			if strings.Contains(got, "8080.0") {
				t.Errorf("%s = %q, rendered an integer as a float", c.call, got)
			}
		})
	}
}

// TestEncodeFuncs_deterministic guards `workspace apply` idempotency: a
// re-render of an unchanged manifest has to produce byte-identical output,
// which for a Go map means the encoder must sort keys rather than follow
// map iteration order.
func TestEncodeFuncs_deterministic(t *testing.T) {
	const call = `enc::json({ z = 1, a = 2, m = 3, b = 4, y = 5, c = 6, x = 7, d = 8 })`
	first := encodeInFileBody(t, call)
	for i := range 20 {
		if got := encodeInFileBody(t, call); got != first {
			t.Fatalf("render %d differs from the first:\ngot:  %q\nwant: %q", i, got, first)
		}
	}
}

// TestEncodeFuncs_outOfRangeInteger pins the accuracy check: big.Float.Int64
// clamps a value beyond int64 while still reporting IsInt, so ignoring its
// accuracy result writes a silently wrong number into a generated document.
func TestEncodeFuncs_outOfRangeInteger(t *testing.T) {
	src := `
service "go" "api" {
  git { url = "github.com/acme/api" }
  file "f" {
    path = "/tmp/f"
    body = enc::json({ n = 99999999999999999999999999 })
  }
  runtime { cmd = ["./api"] }
}
`
	_, err := Compile("test.hcl", []byte(src), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil {
		t.Fatal("Compile succeeded, want an error for a number that does not fit")
	}
	if !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("error = %v, want it to say the number does not fit", err)
	}
}

func TestEncodeFuncs_tomlRejectsNonObject(t *testing.T) {
	src := `
service "go" "api" {
  git { url = "github.com/acme/api" }
  file "f" {
    path = "/tmp/f"
    body = enc::toml([1, 2])
  }
  runtime { cmd = ["./api"] }
}
`
	_, err := Compile("test.hcl", []byte(src), testInv(), nil, testCfgHash, TestConfig{})
	if err == nil {
		t.Fatal("Compile succeeded, want an error for a non-object toml top level")
	}
	if !strings.Contains(err.Error(), "top level must be an object") {
		t.Errorf("error = %v, want it to name the top-level object requirement", err)
	}
}

// encodeInFileBody evaluates one enc:: call through a service file{} body —
// the same path a real manifest takes, so it covers wiring into functions()
// as well as the encoder itself.
func encodeInFileBody(t *testing.T, call string) string {
	t.Helper()
	src := `
service "go" "api" {
  git { url = "github.com/acme/api" }
  file "f" {
    path = "/tmp/f"
    body = ` + call + `
  }
  runtime { cmd = ["./api"] }
}
`
	af := compile(t, src, nil)
	api := svcByName(af, "api")
	if api == nil {
		t.Fatal("service api not resolved")
	}
	if len(api.Runtime.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(api.Runtime.Files))
	}
	return api.Runtime.Files[0].Body
}
