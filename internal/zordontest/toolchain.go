package zordontest

import (
	"flag"
	"os"

	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

const defaultGoVersion = "1.26.2"

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// goVersionFlag is the single source of truth for the Go toolchain
// version conformance fixtures pin (via mise). Drive it per run with:
//
//	go test ./tests/conformance/... -test.toolchain.go.version=1.27.0
//
// Default falls back to $ZORDON_TEST_GO_VERSION then a constant. The
// flag is ALWAYS respected: the version never comes from the caller's
// struct, only from here — what mise actually installs is the property
// worth exercising (the distro matrix alone only varies which Go
// COMPILED zordon: low value, Go is back-compatible, no reflect/ABI).
var goVersionFlag = flag.String(
	"test.toolchain.go.version",
	firstNonEmpty(os.Getenv("ZORDON_TEST_GO_VERSION"), defaultGoVersion),
	"Go toolchain version conformance fixtures pin via mise",
)

// HCL-encode types mirroring zordon's toolchain grammar exactly
// (rootBlock.toolchain -> go block -> required `version`, optional
// `tools`/`env`). WithToolchain SERIALIZES these via gohcl and zordon
// DESERIALIZES the result with its own gohcl decoder when it loads the
// Alphasfile — a real round-trip, never a hand-built string that could
// drift from the schema or bypass the parser.
type tcGoBlock struct {
	Version string            `hcl:"version"`
	Tools   map[string]string `hcl:"tools,optional"`
	Env     map[string]string `hcl:"env,optional"`
}
type tcBlock struct {
	Go *tcGoBlock `hcl:"go,block"`
}
type tcRoot struct {
	Toolchain *tcBlock `hcl:"toolchain,block"`
}

// goToolchainHCL serializes the flag-pinned Go version plus the
// caller's tools/env (the real alphasfile.ToolchainConfig — its
// Version is IGNORED, the flag always wins) into the HCL block
// WithToolchain prepends to an Alphasfile. Read lazily — testing
// parses flags before any test runs, so the flag is set by the time a
// fixture calls WriteFile (NOT valid from a package-var initializer).
func goToolchainHCL(tc alphasfile.ToolchainConfig) string {
	g := &tcGoBlock{Version: *goVersionFlag}
	if len(tc.Tools) > 0 {
		g.Tools = tc.Tools
	}
	if len(tc.Env) > 0 {
		g.Env = map[string]string(tc.Env)
	}
	f := hclwrite.NewEmptyFile()
	gohcl.EncodeIntoBody(&tcRoot{Toolchain: &tcBlock{Go: g}}, f.Body())
	// gohcl has no omitempty: a nil optional map is emitted as
	// `tools = null`, which decodes structurally but is not what
	// zordon's resolver expects. Drop the attrs we didn't set so the
	// block carries only what the caller actually asked for.
	if gb := f.Body().FirstMatchingBlock("toolchain", nil); gb != nil {
		if goB := gb.Body().FirstMatchingBlock("go", nil); goB != nil {
			if g.Tools == nil {
				goB.Body().RemoveAttribute("tools")
			}
			if g.Env == nil {
				goB.Body().RemoveAttribute("env")
			}
		}
	}
	return string(f.Bytes())
}
