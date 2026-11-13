package tools

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/buildquota"
	"github.com/piotrkowalczuk/zordon/internal/zenv"
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

func TestEnsureTools_skipsAToolAlreadyInstalled(t *testing.T) {
	cases := map[string]struct {
		toolchain   string
		present     bool
		wantInstall bool
	}{
		"ruby-present":   {"ruby", true, false},
		"ruby-missing":   {"ruby", false, true},
		"nodejs-present": {"nodejs", true, false},
		"nodejs-missing": {"nodejs", false, true},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			listExit := 1
			if c.present {
				listExit = 0
			}
			bin := filepath.Join(dir, "mise")
			script := "#!/bin/sh\necho \"$@\" >> '" + calls + "'\ncase \"$*\" in *' gem list '*|*' npm ls '*) exit " + strconv.Itoa(listExit) + ";; esac\nexit 0\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			if err := EnsureTools(t.Context(), &buildquota.Limiter{}, nil, bin, dir, c.toolchain, "1.0.0", map[string]string{"tool": "2.0.0"}, io.Discard); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			installed := strings.Contains(string(got), " gem install ") || strings.Contains(string(got), " npm install ")
			if installed != c.wantInstall {
				t.Errorf("installed = %v, want %v; mise was called with:\n%s", installed, c.wantInstall, got)
			}
		})
	}
}

func TestEnsurePackage_skipsAPackageAlreadyInstalled(t *testing.T) {
	cases := map[string]struct {
		present     bool
		wantInstall bool
	}{
		"present": {true, false},
		"missing": {false, true},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			whereExit := 1
			if c.present {
				whereExit = 0
			}
			bin := filepath.Join(dir, "mise")
			script := "#!/bin/sh\necho \"$@\" >> '" + calls + "'\ncase \"$*\" in *' where '*) exit " + strconv.Itoa(whereExit) + ";; esac\nexit 0\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			if err := EnsurePackage(t.Context(), &buildquota.Limiter{}, nil, bin, dir, "aqua:jqlang/jq", "1.7.1", nil, io.Discard); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if installed := strings.Contains(string(got), " install aqua:jqlang/jq@1.7.1"); installed != c.wantInstall {
				t.Errorf("installed = %v, want %v; mise was called with:\n%s", installed, c.wantInstall, got)
			}
		})
	}
}

func TestEnsurePackage_warmStartSkipsHeldLimiterAndColdInjectsBuildEnv(t *testing.T) {
	for _, k := range []string{"CARGO_BUILD_JOBS", "BUNDLE_JOBS", "MAKEFLAGS", "CMAKE_BUILD_PARALLEL_LEVEL", "GOMAXPROCS"} {
		osUnsetForTest(t, k)
	}
	dir := t.TempDir()
	envOut := filepath.Join(dir, "env.txt")
	bin := filepath.Join(dir, "mise")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *' where warm@1.0.0') exit 0;;\n" +
		"  *' where cold@1.0.0') exit 1;;\n" +
		"  *' install cold@1.0.0') printf 'CARGO=%s\\nMAKE=%s\\nBUNDLE=%s\\nCMAKE=%s\\nGO=%s\\n' \"$CARGO_BUILD_JOBS\" \"$MAKEFLAGS\" \"$BUNDLE_JOBS\" \"$CMAKE_BUILD_PARALLEL_LEVEL\" \"$GOMAXPROCS\" > '" + envOut + "'; exit 0;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	lim := buildquota.NewLimiter(3)
	rel, _, ok := lim.TryAcquire()
	if !ok {
		t.Fatal("TryAcquire failed")
	}

	shortCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := EnsurePackage(shortCtx, lim, nil, bin, dir, "warm", "1.0.0", nil, io.Discard); err != nil {
		t.Fatalf("warm EnsurePackage blocked on held limiter or failed: %v", err)
	}

	var phases []string
	waiting := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- EnsurePackage(t.Context(), lim, func(p string) {
			phases = append(phases, p)
			if p == "waiting for build slot" {
				close(waiting)
			}
		}, bin, dir, "cold", "1.0.0", zenv.EnvironmentVariables{"BUNDLE_JOBS": "1"}, io.Discard)
	}()

	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cold EnsurePackage to reach build-slot queue")
	}
	rel()
	if err := <-done; err != nil {
		t.Fatalf("cold EnsurePackage: %v", err)
	}

	rawEnv, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read env output: %v", err)
	}
	gotEnv := string(rawEnv)
	for _, want := range []string{"CARGO=3", "MAKE=-j3", "BUNDLE=1", "CMAKE=3", "GO=3"} {
		if !strings.Contains(gotEnv, want) {
			t.Errorf("install env missing %q in:\n%s", want, gotEnv)
		}
	}
	if len(phases) != 2 || phases[0] != "waiting for build slot" || phases[1] != "installing" {
		t.Errorf("phases = %v, want [waiting for build slot, installing]", phases)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func osUnsetForTest(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
