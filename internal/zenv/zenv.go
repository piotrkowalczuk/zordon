// Package zenv is the project's chokepoint for reading the process
// environment (host env vars, $HOME, .env files). Like zfs, it is
// not a re-export of "os" — it is a small set of use-case-named
// operations: allow-list a slice of host env into a typed map, merge
// overlays, parse dotenv files.
//
// Why a dedicated package: alpha spawns child services with a
// curated env (zordon defaults → host allow-list → toolchain →
// dotenv → user). Building that pipeline in one place keeps the
// precedence rules and the "what leaks from the developer's shell"
// boundary in a single auditable file.
//
// Single-key getters (Get, Lookup) exist so business logic can drop
// `import "os"` without losing access to one-off reads. Per
// AGENTS.md, deep-in-business-logic env reads should stay rare; load
// configuration at startup and pass it down via typed structs.
package zenv

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// EnvironmentVariables is a flat name→value env map. The Join /
// JoinFile methods are non-mutating: each call returns a new map with
// overlays applied in increasing-precedence order (later wins on key
// collision). That lets callers build a spawn env as:
//
//	zenv.FromHost(allow).
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

// PrependPath returns a copy of the receiver with dirs prepended (highest
// priority first) to the PATH-style list variable named by key, joined with
// the OS path-list separator. dirs are de-duplicated preserving first
// occurrence; empty dirs returns an unchanged copy. The receiver is not
// mutated.
//
// This is the surgical alternative to Join for list variables: Join's blunt
// map overlay would replace the whole PATH, dropping the layers below it.
// PrependPath keeps the already-assembled value and only stacks dirs on top —
// e.g. layering a second toolchain's bin dir onto a service's own PATH.
func (e EnvironmentVariables) PrependPath(key string, dirs []string) EnvironmentVariables {
	return e.pathJoin(key, dirs, true)
}

// AppendPath is PrependPath's mirror: dirs go after the existing value
// (lowest priority). Same de-dup and non-mutation contract.
func (e EnvironmentVariables) AppendPath(key string, dirs []string) EnvironmentVariables {
	return e.pathJoin(key, dirs, false)
}

func (e EnvironmentVariables) pathJoin(key string, dirs []string, prepend bool) EnvironmentVariables {
	out := make(EnvironmentVariables, len(e))
	maps.Copy(out, e)
	uniq := dedupStrings(dirs)
	if len(uniq) == 0 {
		return out
	}
	sep := string(zfs.PathListSeparator)
	added := strings.Join(uniq, sep)
	switch cur := out[key]; {
	case cur == "":
		out[key] = added
	case prepend:
		out[key] = added + sep + cur
	default:
		out[key] = cur + sep + added
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
		f, err := zfs.Open(p)
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

// FromHost reads the alpha process environment and keeps only entries
// whose keys are in `allow`. Empty or nil allow → empty result
// (closed-world default).
//
// This is the chokepoint that stops the user's interactive shell —
// mise shims, RUBYLIB, GEM_HOME, BUNDLE_*, PYTHONPATH, CARGO_HOME, etc.
// — from leaking into spawned services. The Alphasfile's `sysenv`
// block is the only way to punch through.
func FromHost(allow []string) EnvironmentVariables {
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

// Environ returns the full host environment as KEY=VAL slice in
// exec.Cmd.Env shape. Use this when a child process needs to inherit
// the parent's env wholesale (typically with a few overrides
// appended). For curated, allow-listed env, use FromHost — that is
// the right tool for spawning user services.
func Environ() []string { return os.Environ() }

// Lookup is the single-key host-env reader exposed for HCL's
// os::env() function — the DSL contract that lets users opt in to
// reading host env vars at config eval time. Outside of that one
// user-facing surface, prefer typed flags bound to env vars via ff
// (the 12-factor pattern) over ad-hoc Lookup calls.
func Lookup(key string) (string, bool) { return os.LookupEnv(key) }

// UserHomeDir returns the current user's home dir (via $HOME on
// Unix). Errors when neither $HOME nor a passwd lookup yields a
// path — callers typically degrade gracefully (e.g. fall back to
// ".zordon" relative to cwd) rather than aborting.
func UserHomeDir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("zenv: user home dir: %w", err)
	}
	return h, nil
}

// dedupStrings returns s with empty entries dropped and later duplicates
// removed, preserving the first occurrence's order.
func dedupStrings(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
