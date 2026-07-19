package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for the Java toolchain: examples/java (spring-petclinic)
// must resolve. Pure Compile — no git clone, no build, no spawn. Java is a
// git-sourced workspace service; the run command is the toolchain default
// (jar inference, resolved alpha-side, so empty here), while the build is
// spelled out only to pass -Dmaven.gitcommitid.skip=true (PetClinic's
// git-commit-id plugin can't read HEAD from zordon's linked worktree).
func TestExampleJavaResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/java/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		StateDir: "/repo/examples/java/workspaces/main",
	}
	af, err := Compile("/repo/examples/java/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	pc := svcByName(af, "petclinic")
	if pc == nil || !pc.Workspaceable() || pc.UseOnly() {
		t.Fatalf("petclinic must be a workspace java service: %+v", pc)
	}
	if pc.Toolchain != ToolchainJava {
		t.Errorf("toolchain = %q, want java", pc.Toolchain)
	}
	if pc.Package == nil || !strings.Contains(pc.Package.Git, "spring-petclinic") {
		t.Errorf("git url not resolved: %+v", pc.Package)
	}
	if pc.Package.Rev == "" {
		t.Errorf("rev pin not resolved: %+v", pc.Package)
	}
	// Explicit build carries the git-commit-id skip flag; run is the
	// toolchain default (jar inference needs the real checkout, so it's
	// deferred to alpha and stays empty at compile time).
	if got := strings.Join(pc.BuildCmd(), " "); !strings.Contains(got, "mvnw") || !strings.Contains(got, "gitcommitid.skip") {
		t.Errorf("build cmd = %q, want it to run ./mvnw with -Dmaven.gitcommitid.skip", got)
	}
	if got := pc.Runtime.Command; len(got) != 0 {
		t.Errorf("runtime cmd = %v, want empty (default jar inference)", got)
	}
}
