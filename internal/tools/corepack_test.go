package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
	"github.com/piotrkowalczuk/zordon/internal/ztest"
)

// TestCorepackMatrix runs the production EnsureNodeCorepack on every Node
// line zordon supports and requires pnpm and yarn to start with no
// `package.json#packageManager` pin, which is what a project with only a
// lockfile gets. npm is excluded because it ships with node and never goes
// through corepack.
//
// Uses the same ZORDON_HOME as conformance (zordontest.DefaultHome →
// `<repo>/.zordon`): CI pre-places mise there and the conformance suite
// shares it as its warm cache.
func TestCorepackMatrix(t *testing.T) {
	nodeVersions := []string{"18.20.5", "20.18.1", "22.11.0", "24.15.0"}
	pms := []string{"pnpm", "yarn"}

	ztest.AssertSystem(t)
	zordonHome := zordontest.DefaultHome(t)
	dataDir := filepath.Join(zordonHome, "toolchain")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin, err := EnsureMise(zordonHome, os.Stderr)
	if err != nil {
		t.Fatalf("EnsureMise: %v", err)
	}

	for _, nv := range nodeVersions {
		t.Run("node-"+nv, func(t *testing.T) {
			release, err := Acquire(dataDir, "node", nv)
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			defer release()

			env, err := MiseEnv(bin, dataDir, "node", nv, os.Stderr)
			if err != nil {
				t.Fatalf("MiseEnv node@%s: %v", nv, err)
			}
			if err := EnsureNodeCorepack(bin, dataDir, nv, env, os.Stderr); err != nil {
				t.Fatalf("EnsureNodeCorepack node@%s: %v", nv, err)
			}
			if home := env["COREPACK_HOME"]; !strings.HasPrefix(home, filepath.Join(dataDir, "node-corepack", nv)+string(filepath.Separator)) {
				t.Errorf("COREPACK_HOME = %q, want a dir private to node %s under %s", home, nv, dataDir)
			}
			cwd := t.TempDir()
			for _, pm := range pms {
				cmd := exec.Command(bin, "exec", "node@"+nv, "--", pm, "--version")
				cmd.Env = overlayEnv(isolatedEnv(dataDir, bin), env)
				cmd.Dir = cwd
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Errorf("node=%s pm=%s: %v\n%s", nv, pm, err, strings.TrimSpace(string(out)))
					continue
				}
				t.Logf("node=%s pm=%s version=%s", nv, pm, lastLine(strings.TrimSpace(string(out))))
			}
		})
	}
}

func TestCorepackSpecFor(t *testing.T) {
	cases := map[string]struct {
		node string
		spec string
		ok   bool
	}{
		"node 18 lts":            {"18.20.5", "corepack@~0.33.0", true},
		"node 18 below floor":    {"18.17.0", "", false},
		"node 20":                {"v20.18.1", "corepack@~0.34.0", true},
		"node 20 below 20.10":    {"20.9.0", "", false},
		"node 21 odd release":    {"21.7.3", "", false},
		"node 22 before 22.22.2": {"22.11.0", "corepack@~0.34.0", true},
		"node 22 from 22.22.2":   {"22.22.2", "corepack@~0.36.0", true},
		"node 24 before 24.15":   {"24.1.0", "corepack@~0.34.0", true},
		"node 24 from 24.15":     {"24.15.0", "corepack@~0.36.0", true},
		"node 26":                {"26.0.0", "corepack@~0.36.0", true},
		"not a version":          {"lts", "", false},
		"two components":         {"22.11", "", false},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			spec, ok := CorepackSpecFor(c.node)
			if spec != c.spec || ok != c.ok {
				t.Errorf("CorepackSpecFor(%q) = (%q, %v), want (%q, %v)", c.node, spec, ok, c.spec, c.ok)
			}
		})
	}
}

// overlayEnv returns base with every key of over set; over wins.
func overlayEnv(base []string, over map[string]string) []string {
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, replaced := over[k]; !replaced {
			out = append(out, kv)
		}
	}
	for k, v := range over {
		out = append(out, k+"="+v)
	}
	return out
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
