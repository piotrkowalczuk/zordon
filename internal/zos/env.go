// Package zos wraps OS-level inputs that zordon composes into a child
// process's environment. Right now it's just EnvironmentVariables and
// helpers to build a layered env for service spawns; future host-
// specific knobs (paths, signals, descriptors) belong here too so the
// rest of the codebase doesn't import os/exec/syscall directly.
package zos

import (
	"bufio"
	"maps"
	"os"
	"strings"
)

// EnvironmentVariables is a flat name→value env map. The Join /
// JoinFile methods are non-mutating: each call returns a new map with
// overlays applied in increasing-precedence order (later wins on key
// collision). That lets callers build a spawn env as:
//
//	zos.NewEnvironmentVariablesFromHost(allow).
//	    Join(toolchain).
//	    JoinFile(dotenvPaths...).
//	    Join(serviceEnv, phaseEnv).
//	    Slice()
//
// where the layers — zordon defaults → host → toolchain → user dotenv
// → user service.env — read left-to-right, lowest-to-highest precedence.
//
// Methods are defined on the value type (not pointer): EnvironmentVariables
// is itself a map and already reference-like, and the no-mutation contract
// is easier to read without `&` everywhere.
type EnvironmentVariables map[string]string

// Join overlays each `other` on top of the receiver in order, returning
// a new map. Later overlays override earlier ones on key collision; nil
// or empty maps are skipped. The receiver is not mutated.
func (e EnvironmentVariables) Join(others ...EnvironmentVariables) EnvironmentVariables {
	out := make(EnvironmentVariables, len(e))
	maps.Copy(out, e)
	for _, o := range others {
		maps.Copy(out, o)
	}
	return out
}

// JoinFile parses each path as a KEY=VAL dotenv file and overlays its
// entries on top of the receiver (later paths win on collision). The
// receiver is not mutated.
//
// Dotenv quirks supported (so it round-trips with the conventions
// users hand-write):
//   - `#` comments and blank lines are skipped.
//   - optional leading `export ` is stripped.
//   - surrounding single or double quotes on the value are stripped.
//
// Values are NOT shell-expanded (no $VAR / `cmd` substitution) — that's
// what the service's env block is for.
//
// Missing or unreadable files are silently skipped — federation lets a
// parent Alphasfile reference a dotenv that only exists in some
// checkouts, and we don't want absence to abort a service spawn.
func (e EnvironmentVariables) JoinFile(paths ...string) EnvironmentVariables {
	out := make(EnvironmentVariables, len(e))
	maps.Copy(out, e)
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
			out[k] = v
		}
		_ = f.Close()
	}
	return out
}

// Slice serializes to the KEY=VAL list shape exec.Cmd.Env expects.
// Order is unspecified — child processes look up by name, not by
// position, so map iteration order is fine.
func (e EnvironmentVariables) Slice() []string {
	out := make([]string, 0, len(e))
	for k, v := range e {
		out = append(out, k+"="+v)
	}
	return out
}

// NewEnvironmentVariablesFromHost reads the alpha process environment
// and keeps only entries whose keys are in `allow`. Empty or nil allow
// → empty result (closed-world default).
//
// This is the chokepoint that stops the user's interactive shell —
// mise shims, RUBYLIB, GEM_HOME, BUNDLE_*, PYTHONPATH, CARGO_HOME, etc.
// — from leaking into spawned services. The Alphasfile's `sysenv`
// block is the only way to punch through.
func NewEnvironmentVariablesFromHost(allow []string) EnvironmentVariables {
	if len(allow) == 0 {
		return EnvironmentVariables{}
	}
	keep := make(map[string]struct{}, len(allow))
	for _, k := range allow {
		keep[k] = struct{}{}
	}
	out := EnvironmentVariables{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, ok := keep[k]; ok {
			out[k] = v
		}
	}
	return out
}
