package zordontest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewProject_createsTmpdirAndHome(t *testing.T) {
	p := NewProject(t)
	if p.Dir() == "" {
		t.Fatal("Dir() empty")
	}
	if st, err := os.Stat(p.Dir()); err != nil || !st.IsDir() {
		t.Errorf("project root not a directory: %v", err)
	}
	if p.Home() == "" {
		t.Fatal("Home() empty — default should produce a path even without options")
	}
}

func TestProject_WriteFile_createsIntermediateDirs(t *testing.T) {
	p := NewProject(t)
	p.WriteFile("a/b/c/file.txt", "hello")

	got, err := os.ReadFile(filepath.Join(p.Dir(), "a/b/c/file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("file content = %q; want %q", got, "hello")
	}
}

func TestProject_CopyTree_copiesFixtureContents(t *testing.T) {
	// Use an existing tree from the repo as the fixture — go.mod
	// is the most reliable thing to have around. We CopyTree the
	// internal/zordontest dir itself (every test repo includes it).
	p := NewProject(t)
	p.CopyTree("internal/zordontest", "harness-src")

	if _, err := os.Stat(filepath.Join(p.Dir(), "harness-src", "project.go")); err != nil {
		t.Errorf("expected copied file missing: %v", err)
	}
}

func TestProject_CopyTree_failsWhenDestExists(t *testing.T) {
	p := NewProject(t)
	p.MkdirAll("dst")

	// Re-implement CopyTree's preconditions in a defer/recover so we
	// don't kill the test with t.Fatal — we want to verify the error
	// path is hit, not propagate it.
	src := filepath.Join(repoRoot(t), "internal/zordontest")
	dst := filepath.Join(p.Dir(), "dst")
	if err := copyTree(src, dst); err == nil {
		t.Errorf("copyTree onto existing dir should fail; got nil error")
	}
}

// REGRESSION: ZORDON_HOME from WithIsolatedHome must override the
// default per-project home. The test harness relies on this for
// scenarios that need a guaranteed-empty home (first-install
// bootstrap, registry-collision tests, etc.).
// REGRESSION: the default ZORDON_HOME for tests must be the shared
// DefaultHome(t) — otherwise every test re-installs mise + toolchain
// from scratch, suite goes from ~seconds to ~hours.
func TestNewProject_defaultHomePointsAtSharedCache(t *testing.T) {
	p := NewProject(t)
	if p.Home() != DefaultHome(t) {
		t.Errorf("Home() = %q; want shared cache %q", p.Home(), DefaultHome(t))
	}
}

func TestWithIsolatedHome_overridesDefault(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "my-iso-home")
	p := NewProject(t, WithIsolatedHome(custom))
	if p.Home() != custom {
		t.Errorf("Home() = %q; want %q from WithIsolatedHome", p.Home(), custom)
	}
}

// REGRESSION: env() always sets ZORDON_HOME so child zordon binaries
// see the harness-controlled home, not the user's real ~/.zordon.
// Without this, every test would pollute the developer's actual
// install with toolchain artifacts.
func TestProject_env_alwaysSetsZordonHome(t *testing.T) {
	p := NewProject(t)
	env := p.env()
	found := ""
	for _, kv := range env {
		if len(kv) > len("ZORDON_HOME=") && kv[:len("ZORDON_HOME=")] == "ZORDON_HOME=" {
			found = kv[len("ZORDON_HOME="):]
		}
	}
	if found != p.Home() {
		t.Errorf("env() ZORDON_HOME = %q; want %q", found, p.Home())
	}
}
