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
// version conformance fixtures pin (via mise) in their generated
// Alphasfiles. Drive it per run with:
//
//	go test -test.toolchain.go.version=1.27.0 ./tests/conformance/...
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

// GoVersion is the resolved Go toolchain version for this run. Call it
// from inside a test — testing runs flag.Parse before any test, so the
// flag value is set by then (NOT valid from a package-var initializer).
func GoVersion() string { return *goVersionFlag }

// GoToolchainHCL renders the `toolchain { go { version = ... } }` block
// fixtures prepend to their Alphasfile, so the pinned version lives in
// exactly one place. Concatenate it onto the manifest body (HCL block
// order is irrelevant), which keeps fixture Sprintf arg lists unchanged.
func GoToolchainHCL() string {
	return fmt.Sprintf("toolchain {\n  go {\n    version = %q\n  }\n}\n", GoVersion())
}
