package alphasfile

import (
	"fmt"
	"strings"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// The test:: namespace lets the conformance harness instrument an
// Alphasfile so it can later assert on what actually executed and in
// what order — without having to wire bespoke logging into every
// golden service.
//
// All test:: functions return SHELL SNIPPETS (string-valued cty),
// suitable to drop into a provision's cmd field, a build cmd, or
// any other slot whose effect is "run this in /bin/sh -c". The
// snippet writes to TestConfig.LogPath when set; the harness keeps
// the path stable across all zordon invocations within one Project
// and reads back via Project.TestLog().
//
// Gating: every function checks TestConfig.Harness at HCL eval time.
// In production builds Harness=false, so calling a test:: function
// fails with a clear "only available in test mode" error — a real-
// world Alphasfile that drops test::log() into production gets a
// config-load failure, not a silent surprise.

// TestConfig configures the test:: HCL functions. cmd/zordon and
// cmd/alpha build it from --test-harness / --test-log flags (auto-
// mapped from ZORDON_TEST_HARNESS / ZORDON_TEST_LOG by ff). Zero
// value disables every test:: call — production-safe by default.
type TestConfig struct {
	Harness bool
	LogPath string
}

// testLogFunc returns `test::log(tag)` — emits the tag (plus newline)
// to cfg.LogPath when the snippet runs. The path is BAKED IN at HCL
// eval time, so spawned services don't need it on their sysenv
// whitelist.
//
// When cfg.LogPath is empty (harness mode but no log path), the
// snippet is a no-op (`true`) so check-style provisions don't
// short-circuit on it.
func testLogFunc(cfg TestConfig) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "tag", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if err := requireHarness(cfg, "test::log"); err != nil {
				return cty.NilVal, err
			}
			tag := args[0].AsString()
			if cfg.LogPath == "" {
				return cty.StringVal("true"), nil
			}
			snippet := fmt.Sprintf("printf '%%s\\n' %s >> %s",
				shellQuote(tag), shellQuote(cfg.LogPath))
			return cty.StringVal(snippet), nil
		},
	})
}

// testFailFunc returns `test::fail(reason)` — emits the reason to
// stderr and exits 1. Drop into a provision cmd that the test
// EXPECTS to be unreachable (e.g., a job downstream of a failing
// dep): if it runs anyway, the alpha log captures the reason and
// the test catches an unexpected non-zero exit.
func testFailFunc(cfg TestConfig) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "reason", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if err := requireHarness(cfg, "test::fail"); err != nil {
				return cty.NilVal, err
			}
			snippet := fmt.Sprintf("printf '%%s\\n' %s >&2; exit 1",
				shellQuote(args[0].AsString()))
			return cty.StringVal(snippet), nil
		},
	})
}

func requireHarness(cfg TestConfig, name string) error {
	if !cfg.Harness {
		return fmt.Errorf("%s() is only available when zordon runs with --test-harness "+
			"(the conformance harness sets this; a production Alphasfile must not call it)",
			name)
	}
	return nil
}

// shellQuote wraps a string in POSIX-safe single quotes so it
// embeds into a `sh -c` snippet as a single literal arg, even when
// the value contains spaces, dollar signs, backticks, or other
// special chars. Existing single quotes are split-and-rejoined
// with `'\”` (the standard POSIX trick).
//
// Pulled out here rather than imported from a util package because
// the test:: snippets are the ONLY place in zordon that needs
// shell-quoting at HCL eval time — keep it local until a second
// caller appears.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
