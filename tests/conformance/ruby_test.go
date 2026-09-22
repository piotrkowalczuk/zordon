//go:build conformance_ruby

package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Ruby conformance: the `service "ruby"` shape (src + default `bundle
// install` + explicit `bundle exec` runtime cmd + HTTP probe) and the
// hermetic gem/bundler environment (#75). Fixture: golden/ruby/echo, a
// stdlib-only echo with an empty Gemfile whose lockfile pins BUNDLED WITH
// 2.5.6 — the same bundler the toolchain declares, so the shim, the
// lockfile and the pin agree and nothing is fetched.
const (
	rubyVersion    = "3.3.6"
	bundlerVersion = "2.5.6"
)

// rubyEcho is echoResponse plus the two Ruby-only fields golden/ruby/echo
// emits.
type rubyEcho struct {
	echoResponse
	BundlerVersion string   `json:"bundler_version"`
	BundlePath     string   `json:"bundle_path"`
	GemPath        []string `json:"gem_path"`
}

// The hermeticity test runs first: on a cold ZORDON_HOME it is the run that
// installs ruby and the declared bundler, so the poison is live for the
// install, not just the spawn. HOME carries a global bundler config that
// redirects the bundle path, a gemrc that would push `gem install` into
// ~/.gem/ruby/<abi> (invisible to the runtime GEM_PATH pin), and the host
// env carries every RubyGems/Bundler knob. A green `bundle exec` with the
// declared bundler proves the pinned interpreter's gem world was used end
// to end and nothing under HOME was read or written.
func TestRubyService_hermetic_hostRubyEnvIgnored(t *testing.T) {
	home := poisonedHome(t,
		homeFile{".bundle/config", "---\nBUNDLE_PATH: \"/poison/bundle\"\nBUNDLE_GEMFILE: \"/poison/Gemfile\"\n"},
		homeFile{".gemrc", "gem: --user-install\n"},
	)

	p := zordontest.NewProject(t)
	p.CopyTree("golden/ruby/echo", "src/echo")
	p.WriteFile("Alphasfile", rubyAlphasfile("echo"))

	startPoisoned(t, p, home, map[string]string{
		"GEM_HOME":       poisonSentinel + "/gems",
		"GEM_PATH":       poisonSentinel + "/gems",
		"BUNDLE_PATH":    poisonSentinel + "/bundle",
		"BUNDLE_GEMFILE": poisonSentinel + "/Gemfile",
		"RUBYOPT":        "-r" + poisonSentinel + "/rc",
	})

	port := p.Get(t, "service.ruby.echo.vars.port").Int()
	echo := mustDecodeRubyEcho(t, port)
	// RUBYOPT is legitimately present: `bundle exec` sets it to load
	// bundler/setup. The sentinel scan inside assertNoPoison is what proves
	// the host value did not survive.
	assertNoPoison(t, echo.Env)
	if echo.BundlerVersion != bundlerVersion {
		t.Errorf("bundler_version = %q, want the declared %s", echo.BundlerVersion, bundlerVersion)
	}
	assertUnderZordonHome(t, p, echo.Env, "BUNDLE_USER_HOME")
	assertGemPathOwned(t, p, echo)
	for _, dir := range echo.GemPath {
		if strings.HasPrefix(dir, home) {
			t.Errorf("gem_path reaches into HOME: %s", dir)
		}
	}
	assertHomeUntouched(t, home, ".bundle/cache", ".gem", ".local/share/gem")
}

// Canonical shape: default build + `bundle exec` runtime. The bundle lives
// out of tree (<state>/bundle/<svc>), the gem world is the mise ruby's, and
// the checkout stays clean — no vendor/bundle, no .bundle/config written.
func TestRubyService_srcDefault_bundleExec(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/ruby/echo", "src/echo")
	p.WriteFile("Alphasfile", rubyAlphasfile("echo"))

	mustStart(t, p)

	port := p.Get(t, "service.ruby.echo.vars.port").Int()
	echo := mustDecodeRubyEcho(t, port)
	if echo.BundlerVersion != bundlerVersion {
		t.Errorf("bundler_version = %q, want %s", echo.BundlerVersion, bundlerVersion)
	}
	if !strings.Contains(echo.RuntimeVersion, rubyVersion) {
		t.Errorf("runtime_version = %q, want pinned ruby %s", echo.RuntimeVersion, rubyVersion)
	}
	wantBundle := filepath.Join(projectDir(t, p), "workspaces", "main", "bundle", "echo")
	if echo.Env["BUNDLE_PATH"] != wantBundle {
		t.Errorf("BUNDLE_PATH = %q, want %q", echo.Env["BUNDLE_PATH"], wantBundle)
	}
	if echo.BundlePath != wantBundle {
		t.Errorf("effective bundle path = %q, want %q", echo.BundlePath, wantBundle)
	}
	assertGemPathOwned(t, p, echo)
	for _, leftover := range []string{"vendor/bundle", ".bundle/config"} {
		if zfs.Exists(filepath.Join(echo.Cwd, leftover)) {
			t.Errorf("build wrote %s into the checkout", leftover)
		}
	}
}

// A checkout's own .bundle/config is project state and stays in force —
// bundler ranks it above the environment, so it overrides zordon's
// BUNDLE_PATH — and the build log says so.
func TestRubyService_localBundleConfigHonored(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/ruby/echo", "src/echo")
	p.WriteFile("src/echo/.bundle/config", "---\nBUNDLE_PATH: \"vendor/bundle\"\n")
	p.WriteFile("Alphasfile", rubyAlphasfile("echo"))

	mustStart(t, p)

	port := p.Get(t, "service.ruby.echo.vars.port").Int()
	echo := mustDecodeRubyEcho(t, port)
	if echo.BundlePath != "vendor/bundle" {
		t.Errorf("effective bundle path = %q, want the checkout's vendor/bundle", echo.BundlePath)
	}
	if !p.AlphaLog().Contains("honoring") || !p.AlphaLog().Contains(".bundle/config") {
		t.Error("alpha log does not report the honored local bundler config")
	}
}

// assertGemPathOwned requires every entry of the service's Gem.path to be
// either the mise ruby's own gem dir or its out-of-tree bundle: `bundle
// exec` rewrites GEM_HOME to the bundle path once one is configured, so the
// search path, not GEM_HOME alone, is what proves no host gem dir is visible.
func assertGemPathOwned(t *testing.T, p *zordontest.Project, echo rubyEcho) {
	t.Helper()
	if len(echo.GemPath) == 0 {
		t.Fatal("gem_path is empty")
	}
	miseRuby := filepath.Join(p.Home(), "toolchain", "installs", "ruby", rubyVersion)
	bundle := filepath.Join(projectDir(t, p), "workspaces", "main", "bundle")
	for _, dir := range echo.GemPath {
		if !strings.HasPrefix(dir, miseRuby) && !strings.HasPrefix(dir, bundle) {
			t.Errorf("gem_path entry %q is neither the mise ruby's gem dir nor the service bundle", dir)
		}
	}
}

// projectDir is p.Dir() with symlinks resolved: alpha reports real paths
// (macOS puts temp dirs under /var → /private/var).
func projectDir(t *testing.T, p *zordontest.Project) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(p.Dir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func rubyAlphasfile(name string) string {
	return fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  ruby {
    version = %q
    tools = { bundler = %q }
  }
}

service "ruby" %q {
  src { path = "./src/%s" }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["bundle", "exec", "ruby", "app.rb", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 100
  }
}
`, rubyVersion, bundlerVersion, name, name)
}

// mustDecodeRubyEcho is mustDecodeEcho for the extended Ruby wire shape;
// same retry rationale (the service served alpha's probe, the test
// connects a beat later from another process).
func mustDecodeRubyEcho(t testing.TB, port int) rubyEcho {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	for {
		var echo rubyEcho
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if readErr == nil && res.StatusCode == http.StatusOK {
				if err := json.Unmarshal(body, &echo); err != nil {
					t.Fatalf("decode ruby echo: %v (body: %s)", err, body)
				}
				return echo
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for the ruby echo: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
