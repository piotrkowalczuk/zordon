package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

type corepackCell struct {
	node, pm, version, note string
	ok                      bool
}

// TestCorepackMatrix probes what Corepack provisions per Node version
// AFTER we refresh Corepack itself to the latest published release.
// Refresh is the planned production behavior: bundled corepack inside
// older Node distributions ships a stale signing-key set, so verifying
// recent pnpm/yarn downloads fails with `Cannot find matching keyid`.
// `npm i -g --prefix <dir> corepack@latest` (run under the pinned
// node) gives that node a fresh corepack with a current keyring.
//
// The test mirrors what production will do at toolchain materialization,
// then asks each PM for its --version with NO `package.json#packageManager`
// pin, so the value we record is exactly Corepack's built-in default
// for that Node version. npm is excluded because it ships bundled
// with node and never goes through corepack.
//
// Uses the same ZORDON_HOME as conformance (zordontest.DefaultHome →
// `<repo>/.zordon`). That dir is what CI pre-places mise into and what
// the conformance suite already shares for its warm cache, so this
// test reuses both without needing to bootstrap mise via cargo.
func TestCorepackMatrix(t *testing.T) {
	nodeVersions := []string{"18.20.5", "20.18.1", "22.11.0"}
	pms := []string{"pnpm", "yarn"}

	zordonHome := zordontest.DefaultHome(t)
	dataDir := filepath.Join(zordonHome, "toolchain")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	bin, err := EnsureMise(zordonHome, os.Stderr)
	if err != nil {
		t.Fatalf("EnsureMise: %v", err)
	}

	// isolatedEnv prepends the mise binary's dir to PATH — the
	// mise-installed node's npm wrapper calls `mise reshim`
	// post-install and exits 127 otherwise.
	env := isolatedEnv(dataDir, bin)

	mustRun := func(label, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Env = env
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}

	var results []corepackCell

	for _, nv := range nodeVersions {
		t.Run("node-"+nv, func(t *testing.T) {
			release, err := Acquire(dataDir, "node", nv)
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			defer release()

			if _, err := MiseEnv(bin, dataDir, "node", nv, os.Stderr); err != nil {
				t.Fatalf("MiseEnv node@%s: %v", nv, err)
			}

			// Per-version playground: where the refreshed corepack
			// lives and where it writes its shims.
			workDir := t.TempDir()
			refreshDir := filepath.Join(workDir, "corepack-latest")
			shimDir := filepath.Join(workDir, "shims")
			emptyCwd := filepath.Join(workDir, "cwd")
			for _, d := range []string{refreshDir, shimDir, emptyCwd} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			spec := "node@" + nv
			mustRun("refresh corepack",
				bin, "exec", spec, "--",
				"npm", "install", "-g",
				"--prefix", refreshDir,
				"corepack@latest")

			refreshedCorepack := filepath.Join(refreshDir, "bin", "corepack")
			mustRun("corepack enable",
				bin, "exec", spec, "--",
				refreshedCorepack, "enable",
				"--install-directory", shimDir)

			t.Logf("refreshed corepack at %s; shims:", refreshedCorepack)
			if entries, _ := os.ReadDir(shimDir); len(entries) > 0 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Logf("  %v", names)
			}

			for _, pm := range pms {
				t.Run(pm, func(t *testing.T) {
					c := corepackCell{node: nv, pm: pm}
					shim := filepath.Join(shimDir, pm)
					if _, statErr := os.Stat(shim); statErr != nil {
						c.note = "no shim at " + shim
						results = append(results, c)
						t.Logf("FAIL node=%s pm=%s: %s", nv, pm, c.note)
						return
					}
					ver := exec.Command(bin, "exec", spec, "--", shim, "--version")
					ver.Env = env
					ver.Dir = emptyCwd
					out, err := ver.CombinedOutput()
					if err != nil {
						c.note = truncate(string(out), 200) + " | " + err.Error()
						results = append(results, c)
						t.Logf("FAIL node=%s pm=%s: %s", nv, pm, c.note)
						return
					}
					c.ok = true
					c.version = lastLine(strings.TrimSpace(string(out)))
					results = append(results, c)
					t.Logf("OK   node=%s pm=%s version=%s", nv, pm, c.version)
				})
			}
		})
	}

	t.Cleanup(func() {
		t.Logf("\n=== Corepack default-version matrix (post-refresh) ===\n%s",
			renderTable(results))
	})
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " // ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func renderTable(rows []corepackCell) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s %-6s %-12s %-6s %s\n", "node", "pm", "version", "status", "note")
	for _, r := range rows {
		status := "ok"
		if !r.ok {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "%-10s %-6s %-12s %-6s %s\n", r.node, r.pm, r.version, status, r.note)
	}
	return b.String()
}
