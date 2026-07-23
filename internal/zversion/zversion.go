// Package zversion carries the build identity every zordon binary reports.
//
// There are two ways a zordon binary comes into existence, and they stamp
// their identity differently:
//
//   - A release build (see .goreleaser.yaml) sets the three vars below at
//     link time with -ldflags -X, so the version is exactly the tag that
//     was released, independent of whatever VCS state the runner happened
//     to check out.
//   - Everything else — `go build`, `go install ...@latest`, `make build` —
//     leaves them empty, and Go's own build info takes over: the module
//     version for an `@version` install, the VCS revision otherwise.
//
// The fallback matters: without it a locally built binary would either
// claim to be a release it isn't, or report nothing at all. Both make a
// bug report useless, which is the main thing --version exists for.
package zversion

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Info is one binary's build identity.
type Info struct {
	Version string // release tag, module version, or "(devel)"
	Commit  string // short revision, suffixed "+dirty" when the tree was modified
	Date    string // RFC3339 commit time, empty when unknown
	Go      string // toolchain that produced the binary
}

// Get resolves this binary's identity.
func Get() Info { return resolve(version, commit, date, debug.ReadBuildInfo) }

// Line renders the single line each binary prints for --version:
//
//	zordon v0.18.0 (9b4437bd1c2e, 2026-07-23T20:00:00Z, go1.26.5)
func Line(name string) string { return name + " " + Get().String() }

func (i Info) String() string {
	var detail []string
	for _, part := range []string{i.Commit, i.Date, i.Go} {
		if part != "" {
			detail = append(detail, part)
		}
	}
	if len(detail) == 0 {
		return i.Version
	}
	return fmt.Sprintf("%s (%s)", i.Version, strings.Join(detail, ", "))
}

// Stamped by the release build via -ldflags -X; empty in every other build.
var (
	version string
	commit  string
	date    string
)

const shortCommitLen = 12

// resolve takes the build-info reader as a parameter so the whole fallback
// ladder is exercisable in tests without an actual linker stamp.
func resolve(ldVersion, ldCommit, ldDate string, read func() (*debug.BuildInfo, bool)) Info {
	out := Info{Version: ldVersion, Commit: ldCommit, Date: ldDate, Go: runtime.Version()}

	if info, ok := read(); ok && info != nil {
		if out.Version == "" {
			out.Version = info.Main.Version
		}
		var modified bool
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if out.Commit == "" {
					out.Commit = setting.Value
				}
			case "vcs.time":
				if out.Date == "" {
					out.Date = setting.Value
				}
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
		if len(out.Commit) > shortCommitLen {
			out.Commit = out.Commit[:shortCommitLen]
		}
		if modified && out.Commit != "" {
			out.Commit += "+dirty"
		}
	}

	if out.Version == "" {
		out.Version = "(devel)"
	}
	return out
}
