package main

import (
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

// resolveBarrier extends a 3-way grammar (toolchain / build / runtime /
// provision). These tests exercise the new branches without spinning
// up a real bringup goroutine: they hand-craft the relevant ctxs into
// alphaState and check resolution.

func newStateWithToolchain(t *testing.T) *alphaState {
	t.Helper()
	s := &alphaState{}
	s.addToolchain(newToolchainCtx("ruby", "3.3.10", nil, nil))
	return s
}

func TestResolveBarrier_toolchain(t *testing.T) {
	s := newStateWithToolchain(t)
	bt, err := s.resolveBarrier("toolchain.ruby@ready")
	if err != nil {
		t.Fatalf("toolchain.ruby@ready: %v", err)
	}
	if bt.target == nil || bt.fail == nil {
		t.Fatal("resolveBarrier returned nil target or fail")
	}
	// Unknown lang must error — alpha should fail loudly, not silently
	// gate on a never-firing barrier.
	if _, err := s.resolveBarrier("toolchain.go@ready"); err == nil {
		t.Error("toolchain.go@ready resolved despite no `go` entity")
	}
	// Unknown state on a known toolchain errors out.
	if _, err := s.resolveBarrier("toolchain.ruby@galaxybrain"); err == nil {
		t.Error("toolchain.ruby@galaxybrain resolved despite unknown state")
	}
}

func TestResolveBarrier_serviceBuild(t *testing.T) {
	s := &alphaState{}
	sc := newServiceCtx("api", "ruby")
	s.addService(sc)

	bt, err := s.resolveBarrier("service.ruby.api.build@success")
	if err != nil {
		t.Fatalf("service.ruby.api.build@success: %v", err)
	}
	if bt.target == nil || bt.fail == nil {
		t.Fatal("nil target/fail for build@success")
	}
	// Build ref must dispatch to the build instance, not the runtime
	// one — verify by checking that a build.Reach("success") closes
	// the target.
	sc.build.Reach("success")
	select {
	case <-bt.target.Wait():
	default:
		t.Error("build.Reach(success) didn't close the target barrier")
	}
	// Sanity: same ref pattern with `.runtime` still resolves and
	// targets the runtime lifecycle, not build.
	rt, err := s.resolveBarrier("service.ruby.api.runtime@ready")
	if err != nil {
		t.Fatalf("service.ruby.api.runtime@ready: %v", err)
	}
	sc.lifecycle.Reach("ready")
	select {
	case <-rt.target.Wait():
	default:
		t.Error("lifecycle.Reach(ready) didn't close the runtime target")
	}
}

func TestImplicitRuntimeAfter_prependsToolchainDep(t *testing.T) {
	s := &alphaState{}
	s.addToolchain(newToolchainCtx("ruby", "3.3.10", nil, nil))

	// Service uses ruby AND has its own explicit deps.
	svc := &alphasfile.Service{
		Toolchain: "ruby",
		Runtime: &alphasfile.RuntimeConfig{
			Name:  "api",
			After: []string{"service.ruby.db.runtime@ready"},
		},
	}
	got := implicitRuntimeAfter(svc, s)
	if len(got) != 2 {
		t.Fatalf("expected 2 deps (implicit toolchain + 1 explicit), got %v", got)
	}
	if got[0] != "toolchain.ruby@ready" {
		t.Errorf("first dep should be implicit toolchain.ruby@ready, got %q", got[0])
	}
	if got[1] != "service.ruby.db.runtime@ready" {
		t.Errorf("second dep should be explicit, got %q", got[1])
	}
}

func TestImplicitRuntimeAfter_noToolchainNoImplicit(t *testing.T) {
	s := &alphaState{}
	svc := &alphasfile.Service{
		Toolchain: "ruby",
		Runtime: &alphasfile.RuntimeConfig{
			Name:  "api",
			After: []string{"service.ruby.db.runtime@ready"},
		},
	}
	got := implicitRuntimeAfter(svc, s)
	for _, ref := range got {
		if strings.HasPrefix(ref, "toolchain.") {
			t.Errorf("implicit toolchain dep injected without an entity: %q", ref)
		}
	}
}
