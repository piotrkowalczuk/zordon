package zordontest

import (
	"flag"
	"fmt"
	"os"
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
// Default falls back to $ZORDON_TEST_GO_VERSION then a constant, so CI
// can vary the mise-provisioned Go by flag OR env without touching
// fixtures. That version — what mise actually installs — is the
// property worth exercising; the distro matrix alone only varies which
// Go COMPILED zordon (low value: Go is back-compatible, no reflect/ABI
// surface).
var goVersionFlag = flag.String(
	"test.toolchain.go.version",
	firstNonEmpty(os.Getenv("ZORDON_TEST_GO_VERSION"), defaultGoVersion),
	"Go toolchain version conformance fixtures pin via mise",
)

// goToolchainHCL renders the `toolchain { go { version = ... } }` block
// WithToolchain prepends to an Alphasfile. Read lazily (testing parses
// flags before any test runs, so the flag value is set by the time a
// test calls WriteFile — NOT valid from a package-var initializer).
func goToolchainHCL() string {
	return fmt.Sprintf("toolchain {\n  go {\n    version = %q\n  }\n}\n", *goVersionFlag)
}
