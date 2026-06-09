package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for the pkg toolchain: examples/pkg must resolve, the
// package coordinate must fold to a mise spec, no source/build is
// attached, and TCP readiness resolves to the picked port. Pure Compile —
// no mise install, no spawn.
func TestExamplePkgResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/pkg/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		StateDir: "/repo/examples/pkg/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/pkg/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	redis := svcByName(af, "redis")
	if redis == nil {
		t.Fatal("service redis not resolved")
	}
	if redis.Package != nil {
		t.Errorf("pkg service must not carry a Package source: %+v", redis.Package)
	}
	if redis.Pkg == nil || redis.Pkg.Name != "redis" || redis.Pkg.Version != "7.4.1" {
		t.Fatalf("redis.Pkg = %+v, want {redis 7.4.1}", redis.Pkg)
	}
	if redis.Worktreeable() || redis.UseOnly() || redis.Buildable() {
		t.Errorf("pkg service must not be worktreeable/use-only/buildable")
	}
	if redis.Runtime.Readiness == nil || redis.Runtime.Readiness.TCP == nil || redis.Runtime.Readiness.TCP.Port <= 0 {
		t.Errorf("redis must have a resolved TCP readiness probe: %+v", redis.Runtime.Readiness)
	}
	if got := strings.Join(redis.Runtime.Command, " "); !strings.Contains(got, "redis-server --port ") {
		t.Errorf("redis cmd not resolved: %v", redis.Runtime.Command)
	}

	pg := svcByName(af, "postgres")
	if pg == nil || pg.Pkg == nil || pg.Pkg.Name != "asdf:mise-plugins/mise-postgres" || pg.Pkg.Version != "16.4" {
		t.Fatalf("postgres.Pkg = %+v", pg.Pkg)
	}
	// build { env } flows into BuildEnv → injected into the mise install.
	if pg.Runtime.BuildEnv["POSTGRES_EXTRA_CONFIGURE_OPTIONS"] != "--without-icu" {
		t.Errorf("postgres build env not resolved: %v", pg.Runtime.BuildEnv)
	}
	// initdb runs before the service cmd (runtime.after gates on it).
	if len(pg.Runtime.After) == 0 || !strings.Contains(pg.Runtime.After[0], "provision.initdb") {
		t.Errorf("postgres runtime.after must gate on the initdb provision: %v", pg.Runtime.After)
	}
}
