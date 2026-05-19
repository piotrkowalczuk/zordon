package zordontest

import (
	"fmt"
	"os"
)

// Toolchain is the single source of truth for the language toolchain
// versions conformance fixtures pin (via mise) in their generated
// Alphasfiles. Centralizing it here lets the CI matrix drive what Go
// version mise actually provisions — the property worth testing —
// instead of every fixture hardcoding one string.
//
// Rationale: the distro matrix's *only* toolchain effect otherwise is
// which Go COMPILED zordon, which is low-value (Go is back-compatible
// and zordon exposes no reflect/ABI surface). What's worth exercising
// is mise + the internal toolchain zordon provisions for services —
// so make that version injectable.
//
// Override per-process with $ZORDON_TEST_GO_VERSION so a CI job can
// exercise a different mise-provisioned Go without editing fixtures.
var Toolchain = struct {
	Go struct{ Version string }
}{}

const defaultGoVersion = "1.26.2"

func init() {
	Toolchain.Go.Version = defaultGoVersion
	if v := os.Getenv("ZORDON_TEST_GO_VERSION"); v != "" {
		Toolchain.Go.Version = v
	}
}

// GoToolchainHCL renders the `toolchain { go { version = ... } }` block
// fixtures prepend to their Alphasfile, so the pinned version lives in
// exactly one place. Concatenate it onto the manifest body (HCL block
// order is irrelevant), which keeps fixture Sprintf arg lists unchanged.
func GoToolchainHCL() string {
	return fmt.Sprintf("toolchain {\n  go {\n    version = %q\n  }\n}\n", Toolchain.Go.Version)
}
