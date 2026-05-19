package tools

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ResolveDataDir is the only piece of the package that's pure-Go (no
// subprocess) and the most important to lock in: it expresses the
// federation-like walk-up that lets agent-installed local overrides
// shadow the global cache.

func TestResolveDataDir_walkUpFindsNearestExisting(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "work", "proj")
	deep := filepath.Join(project, "sub", "deeper")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	// Place a "go 1.25" install at project root only.
	mustMkdir(t, filepath.Join(project, ".zordon", "toolchain", "installs", "go", "1.25.6"))

	got := ResolveDataDir(deep, filepath.Join(home, ".zordon", "toolchain"), "go", "1.25.6")
	want := filepath.Join(project, ".zordon", "toolchain")
	if got != want {
		t.Errorf("ResolveDataDir = %s; want project-level %s", got, want)
	}
}

func TestResolveDataDir_innerOverridesOuter(t *testing.T) {
	// Two levels of override: home has 1.25.6, project also has 1.25.6.
	// The closer (project) wins.
	home := t.TempDir()
	project := filepath.Join(home, "work", "proj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(home, ".zordon", "toolchain", "installs", "go", "1.25.6"))
	mustMkdir(t, filepath.Join(project, ".zordon", "toolchain", "installs", "go", "1.25.6"))

	got := ResolveDataDir(project, filepath.Join(home, ".zordon", "toolchain"), "go", "1.25.6")
	want := filepath.Join(project, ".zordon", "toolchain")
	if got != want {
		t.Errorf("ResolveDataDir = %s; want closest %s", got, want)
	}
}

func TestResolveDataDir_noInstallReturnsHomeDefault(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "work", "proj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	// Nothing pre-installed anywhere.
	defaultDD := filepath.Join(home, ".zordon", "toolchain")
	got := ResolveDataDir(project, defaultDD, "go", "1.25.6")
	if got != defaultDD {
		t.Errorf("ResolveDataDir = %s; want defaultDataDir %s", got, defaultDD)
	}
}

func TestResolveDataDir_versionSpecific(t *testing.T) {
	// 1.25.6 installed at project, 1.26.0 NOT installed. Resolving
	// 1.26.0 must skip the 1.25.6 directory and fall back to default.
	home := t.TempDir()
	project := filepath.Join(home, "work", "proj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(project, ".zordon", "toolchain", "installs", "go", "1.25.6"))

	defaultDD := filepath.Join(home, ".zordon", "toolchain")
	got := ResolveDataDir(project, defaultDD, "go", "1.26.0")
	want := defaultDD
	if got != want {
		t.Errorf("ResolveDataDir for 1.26.0 = %s; want home default %s "+
			"(local 1.25.6 must not match)", got, want)
	}
}

// REGRESSION: with `from` pointing at a test-cache dir, walk-up MUST
// NOT escape past defaultDataDir's parent. Without the bound, a real
// install in the developer's `~/.zordon/toolchain/installs/<tool>/<ver>`
// would be found by the walk and silently reused — tests would
// pollute the developer's actual zordon install.
//
// Simulated layout (no real ~/.zordon needed): a user-home variant
// further up the tree must not be reached from below the bound.
func TestResolveDataDir_doesNotLeakAboveDefaultDataDir(t *testing.T) {
	// Simulated layout:
	//   <tmp>/userhome/.zordon/toolchain/installs/go/1.25.6  ← shouldn't be found
	//   <tmp>/userhome/projects/repo/.zordon/                ← bound (defaultDataDir's parent)
	//                  /toolchain                            ← defaultDataDir
	tmp := t.TempDir()
	userHome := filepath.Join(tmp, "userhome")
	repo := filepath.Join(userHome, "projects", "repo")
	testCache := filepath.Join(repo, ".zordon")
	if err := os.MkdirAll(testCache, 0o755); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(userHome, ".zordon", "toolchain", "installs", "go", "1.25.6"))

	defaultDD := filepath.Join(testCache, "toolchain")
	got := ResolveDataDir(testCache, defaultDD, "go", "1.25.6")
	if got != defaultDD {
		t.Errorf("walk-up leaked above bound: ResolveDataDir = %s; want %s", got, defaultDD)
	}
}

// TestAcquire_serializesSameKey is the contract test for the file lock:
// two goroutines requesting the same (tool, version) MUST run their
// critical sections sequentially, not interleaved. This is what guards
// concurrent `gem install bundler` calls from corrupting the shared
// tarball-extract under installs/ruby/<ver>/.
func TestAcquire_serializesSameKey(t *testing.T) {
	dataDir := t.TempDir()

	var inside atomic.Int32
	var maxInside atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			release, err := Acquire(dataDir, "ruby", "3.3.10")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer release()
			n := inside.Add(1)
			for {
				m := maxInside.Load()
				if n <= m || maxInside.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond) // make any interleave visible
			inside.Add(-1)
		})
	}
	wg.Wait()
	if got := maxInside.Load(); got != 1 {
		t.Errorf("max concurrent holders = %d; want 1", got)
	}
}

// TestAcquire_independentKeysParallelize asserts the granularity is
// (tool, version) — two different versions don't block each other.
func TestAcquire_independentKeysParallelize(t *testing.T) {
	dataDir := t.TempDir()

	r1, err := Acquire(dataDir, "ruby", "3.0.7")
	if err != nil {
		t.Fatal(err)
	}
	defer r1()

	// Should NOT block: different version is a different lock.
	done := make(chan struct{})
	go func() {
		r2, err := Acquire(dataDir, "ruby", "3.3.10")
		if err != nil {
			t.Errorf("Acquire(3.3.10): %v", err)
			return
		}
		r2()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Acquire on independent (ruby,3.3.10) blocked behind (ruby,3.0.7)")
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}
