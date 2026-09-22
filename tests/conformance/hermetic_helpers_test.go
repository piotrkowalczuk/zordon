package conformance_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Hermeticity helpers shared by the per-toolchain hermetic_<lang>_test.go
// files (untagged, like shared_test.go, so every bucket compiles them).
//
// The pattern: a throwaway HOME seeded with the ambient files a toolchain's
// tooling would read — each poisoned so the build breaks if it is read —
// plus env poison on the `zordon start` invocation. A green bringup proves
// neither the files nor the env reached the install or the service; the
// env assertions prove the relocations point under ZORDON_HOME; the
// home assertions prove nothing was written back into HOME.

// poisonSentinel is the value every env-var poison carries; assertNoPoison
// scans for it so a poisoned key that got renamed on the way through still
// trips the check.
const poisonSentinel = "/poison"

type homeFile struct {
	rel     string
	content string
}

// poisonedHome writes files under a fresh temp dir and returns it as the
// HOME to hand the bringup.
func poisonedHome(t *testing.T, files ...homeFile) string {
	t.Helper()
	home := t.TempDir()
	for _, f := range files {
		path := filepath.Join(home, f.rel)
		if err := zfs.EnsureDir(filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
		if err := zfs.AtomicWrite(path, []byte(f.content)); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// startPoisoned runs `zordon start` with HOME swapped for home and every
// poison KEY=VALUE set on the invocation, and requires it to come up.
func startPoisoned(t *testing.T, p *zordontest.Project, home string, poison map[string]string) {
	t.Helper()
	opts := []zordontest.StartOption{zordontest.StartEnv("HOME", home)}
	for k, v := range poison {
		opts = append(opts, zordontest.StartEnv(k, v))
	}
	p.Start(t, opts...).OK()
}

// assertNoPoison fails on any of keys present in env, and on any value
// anywhere in env that carries the poison sentinel.
func assertNoPoison(t *testing.T, env map[string]string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := env[k]; ok {
			t.Errorf("poisoned host %s=%q reached the service", k, v)
		}
	}
	for k, v := range env {
		if strings.Contains(v, poisonSentinel) {
			t.Errorf("poison sentinel leaked through %s=%q", k, v)
		}
	}
}

// assertUnderZordonHome requires env[key] to point below the project's
// ZORDON_HOME toolchain dir — the zordon-owned relocation target.
func assertUnderZordonHome(t *testing.T, p *zordontest.Project, env map[string]string, key string) {
	t.Helper()
	want := filepath.Join(p.Home(), "toolchain")
	if v := env[key]; !strings.HasPrefix(v, want) {
		t.Errorf("%s = %q, want a path under %s", key, v, want)
	}
}

// assertHomeUntouched fails if any of rel exists under home — the caches
// and state files a non-hermetic toolchain would have written there.
func assertHomeUntouched(t *testing.T, home string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		if zfs.Exists(filepath.Join(home, r)) {
			t.Errorf("toolchain wrote into HOME: %s", filepath.Join(home, r))
		}
	}
}

// envFromDump parses the output of `env` (KEY=VALUE per line) captured by
// a provision, for services whose runtime cannot echo its env back.
func envFromDump(body string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(body, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}
