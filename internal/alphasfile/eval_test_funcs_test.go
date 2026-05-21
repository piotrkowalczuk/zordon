package alphasfile

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestTestLogFunc_returnsAppendSnippetWhenHarnessSet(t *testing.T) {
	cfg := TestConfig{Harness: true, LogPath: "/tmp/observed.log"}
	got, err := testLogFunc(cfg).Call([]cty.Value{cty.StringVal("p1-ran")})
	if err != nil {
		t.Fatal(err)
	}
	snippet := got.AsString()
	if !strings.Contains(snippet, "'p1-ran'") {
		t.Errorf("snippet missing quoted tag: %q", snippet)
	}
	if !strings.Contains(snippet, "'/tmp/observed.log'") {
		t.Errorf("snippet missing quoted log path: %q", snippet)
	}
	if !strings.Contains(snippet, ">>") {
		t.Errorf("snippet must append (>>), not overwrite: %q", snippet)
	}
}

// REGRESSION: without a log path the snippet must be a shell no-op
// — not unset, not the literal env-var expansion. Provisions that
// chain on `check && cmd` would break if test::log returned `:` (empty
// command) since some shells treat that specially with `&&`.
func TestTestLogFunc_noopSnippetWhenLogUnset(t *testing.T) {
	cfg := TestConfig{Harness: true}
	got, err := testLogFunc(cfg).Call([]cty.Value{cty.StringVal("p1-ran")})
	if err != nil {
		t.Fatal(err)
	}
	if got.AsString() != "true" {
		t.Errorf("no-op snippet = %q; want %q", got.AsString(), "true")
	}
}

// REGRESSION: outside harness mode the function MUST error out with
// a clear message rather than silently returning a snippet — that's
// how production Alphasfiles that accidentally use test::log get
// caught at config load.
func TestTestLogFunc_errorsOutsideHarness(t *testing.T) {
	cfg := TestConfig{} // Harness defaults to false
	_, err := testLogFunc(cfg).Call([]cty.Value{cty.StringVal("p1-ran")})
	if err == nil {
		t.Fatal("test::log() outside harness should error; got nil")
	}
	if !strings.Contains(err.Error(), "--test-harness") {
		t.Errorf("error should mention the flag; got %q", err)
	}
}

// REGRESSION: tags with single quotes must survive — POSIX shell
// quoting puts the original string inside single quotes, so an inner
// single quote needs splitting into `'\''`. Without this fix a
// provision with cmd = test::log("can't do that") would produce
// invalid shell.
func TestTestLogFunc_quotesEmbeddedSingleQuotes(t *testing.T) {
	cfg := TestConfig{Harness: true, LogPath: "/tmp/x"}
	got, err := testLogFunc(cfg).Call([]cty.Value{cty.StringVal("can't")})
	if err != nil {
		t.Fatal(err)
	}
	// Expect POSIX-safe quoting: outer single quotes, embedded
	// quote escaped as `'\''`.
	if !strings.Contains(got.AsString(), `'can'\''t'`) {
		t.Errorf("embedded single quote not escaped POSIX-style: %q", got.AsString())
	}
}

func TestTestFailFunc_returnsExitSnippetWhenHarnessSet(t *testing.T) {
	cfg := TestConfig{Harness: true}
	got, err := testFailFunc(cfg).Call([]cty.Value{cty.StringVal("unreachable")})
	if err != nil {
		t.Fatal(err)
	}
	snippet := got.AsString()
	if !strings.Contains(snippet, "'unreachable'") {
		t.Errorf("snippet missing quoted reason: %q", snippet)
	}
	if !strings.Contains(snippet, "exit 1") {
		t.Errorf("snippet must exit non-zero: %q", snippet)
	}
	if !strings.Contains(snippet, ">&2") {
		t.Errorf("reason should go to stderr (>&2): %q", snippet)
	}
}

func TestTestFailFunc_errorsOutsideHarness(t *testing.T) {
	cfg := TestConfig{}
	_, err := testFailFunc(cfg).Call([]cty.Value{cty.StringVal("x")})
	if err == nil {
		t.Fatal("test::fail() outside harness should error; got nil")
	}
}

// Quoting helper — covered through the public functions above but
// pinned here too so a refactor that breaks the algorithm trips an
// obvious test name.
func TestShellQuote_wrapsAndEscapes(t *testing.T) {
	cases := map[string]string{
		"plain":   `'plain'`,
		"with sp": `'with sp'`,
		`it's`:    `'it'\''s'`,
		"$HOME":   `'$HOME'`,
		"`cmd`":   "'`cmd`'",
		"a\nb":    "'a\nb'", // newlines pass through; inside '...' they're literal
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q; want %q", in, got, want)
		}
	}
}
