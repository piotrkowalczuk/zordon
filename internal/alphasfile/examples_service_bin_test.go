package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for the cross-service binary case: examples/service_bin
// must resolve, redis's `backup` provision must gate on postgres readiness,
// and its PATH env must be a prepend directive wrapping postgres's
// fs::service::bin sentinel (so at runtime alpha layers postgres's bin dir
// onto the provision's PATH). Pure Compile — no mise install, no spawn.
func TestExampleServiceBinResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/service_bin/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		StateDir: "/repo/examples/service_bin/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/service_bin/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	redis := svcByName(af, "redis")
	if redis == nil || redis.Runtime == nil || len(redis.Runtime.Provision) != 1 {
		t.Fatalf("redis backup provision not resolved: %+v", redis)
	}
	backup := redis.Runtime.Provision[0]
	if backup.Name != "backup" {
		t.Fatalf("provision name = %q, want backup", backup.Name)
	}
	// Must gate on postgres readiness — the binary's toolchain has to be up.
	if len(backup.After) == 0 || !strings.Contains(backup.After[0], "service.pkg.postgres.runtime") {
		t.Errorf("backup.after must gate on postgres readiness: %v", backup.After)
	}

	// PATH carries a prepend directive; substituting the postgres svcbin
	// sentinel (stand-in resolver) and parsing yields the dir as the sole
	// prepend arg — the same chain alpha runs at provision time.
	resolved := SubstituteBins(backup.Env["PATH"], func(kind, ref string) []string {
		if kind == "svc" && ref == serviceID("pkg", "postgres") {
			return []string{"/install/postgres/bin"}
		}
		return nil
	})
	op, args, ok := ParseEnvOp(resolved)
	if !ok || op != "prepend" {
		t.Fatalf("PATH is not a prepend directive: %q (ok=%v op=%q)", backup.Env["PATH"], ok, op)
	}
	if len(args) != 1 || args[0] != "/install/postgres/bin" {
		t.Fatalf("prepend args = %q, want [/install/postgres/bin]", args)
	}
}
