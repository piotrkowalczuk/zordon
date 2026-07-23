package main

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/piotrkowalczuk/zordon/internal/zversion"
)

// mcpImplementation is what the MCP server announces itself as during
// initialize, so a connected agent sees the same version the CLI prints.
func mcpImplementation() *mcp.Implementation {
	return &mcp.Implementation{Name: "zordon", Title: "Zordon", Version: zversion.Get().Version}
}

// versionRequested reports whether argv is asking for the build identity.
//
// This is checked before ff parses anything, which is why --version is not a
// regular root flag: ff.WithEnvVarPrefix("ZORDON") binds every long flag to
// $ZORDON_<NAME>, so a --version flag would answer to $ZORDON_VERSION — a
// natural name for pinning an install in a Dockerfile or CI. Exporting it
// would then fail bool parsing and take down every zordon command, not just
// this one.
func versionRequested(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-V", "--version", "version":
		return true
	}
	return false
}

// skewWarning returns a non-empty warning when zordon and the alpha it just
// spawned are different builds — almost always a stale alpha sitting earlier
// on $PATH than a freshly installed one. Deliberately advisory: the control
// protocol is stable across patch releases, so refusing to run would be a
// worse outcome than a mismatched pair that works.
func skewWarning(self, peer string) string {
	if peer == "" || peer == self {
		return ""
	}
	return "version skew: zordon " + self + " spawned alpha " + peer +
		" (check $PATH, or pin one with `zordon start --alpha /path/to/alpha`)"
}
