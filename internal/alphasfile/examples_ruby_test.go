package alphasfile

import (
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// Regression oracle for the Ruby toolchain. Pure Compile — no bundler,
// no spawn. Ruby has no inferred binary: `cmd` is required; the build is
// the toolchain default (`bundle install`, no build block in the example)
// and the bundle dir is the per-service out-of-tree path BUNDLE_PATH
// points at.
func TestExampleRubyResolves(t *testing.T) {
	b, err := zfs.Read("../../examples/ruby/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		StateDir: "/repo/examples/ruby/workspaces/main",
	}
	af, err := Compile("/repo/examples/ruby/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	app := svcByName(af, "app")
	if app == nil || !app.Workspaceable() || app.UseOnly() {
		t.Fatalf("app must be a workspace ruby service: %+v", app)
	}
	if app.Toolchain != ToolchainRuby {
		t.Errorf("toolchain = %q, want ruby", app.Toolchain)
	}
	if got := app.BuildCmd(); len(got) != 0 {
		t.Errorf("build cmd = %v, want none (toolchain default bundle install)", got)
	}
	if got := strings.Join(app.Runtime.Command, " "); !strings.HasPrefix(got, "bundle exec ruby app.rb -addr 127.0.0.1:") {
		t.Errorf("app cmd not resolved: %v", app.Runtime.Command)
	}
	if got, want := app.Runtime.BundleDir, "/repo/examples/ruby/workspaces/main/bundle/app"; got != want {
		t.Errorf("bundle dir = %q, want %q", got, want)
	}
	if tc := af.Toolchain[ToolchainRuby]; tc == nil || tc.Tools["bundler"] != "2.5.6" {
		t.Errorf("toolchain.ruby.tools.bundler not pinned: %+v", af.Toolchain[ToolchainRuby])
	}
}

// Only ruby services carry a bundle dir; the field stays empty for the
// other toolchains so the per-service env slice is never emitted for them.
func TestExampleGoHasNoBundleDir(t *testing.T) {
	b, err := zfs.Read("../../examples/go/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		StateDir: "/repo/examples/go/workspaces/main",
	}
	af, err := Compile("/repo/examples/go/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, s := range af.All() {
		if s.Runtime != nil && s.Runtime.BundleDir != "" {
			t.Errorf("service %s (%s) has a bundle dir: %q", s.Name(), s.Toolchain, s.Runtime.BundleDir)
		}
	}
}
