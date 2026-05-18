package alphasfile

import (
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/lifecycle"
)

// Each Def in this package is exercised by other packages (alpha
// drives the actual Reach transitions), but the *shape* — what states
// exist, what precedence edges chain them — is part of the wire
// contract: a federation parent that exposes `service.X.build@success`
// must still resolve in a child built against an older binary. These
// tests pin that shape.

func TestToolchainLifecycle_shape(t *testing.T) {
	for _, s := range []lifecycle.State{"installing", "ready", "failed"} {
		if !ToolchainLifecycle.Has(s) {
			t.Errorf("ToolchainLifecycle missing state %q", s)
		}
	}
	// Reaching `ready` must imply `installing` was reached; reaching
	// `failed` must imply `installing` was reached — both terminals
	// share the same precondition.
	in := lifecycle.NewInstance(ToolchainLifecycle)
	in.Reach("ready")
	if !in.Reached("installing") {
		t.Error("Reach(ready) did not imply installing")
	}
	in2 := lifecycle.NewInstance(ToolchainLifecycle)
	in2.Reach("failed")
	if !in2.Reached("installing") {
		t.Error("Reach(failed) did not imply installing")
	}
	if in2.Reached("ready") {
		t.Error("Reach(failed) leaked into ready")
	}
}

func TestBuildLifecycle_shape(t *testing.T) {
	for _, s := range []lifecycle.State{"scheduled", "running", "success", "failure"} {
		if !BuildLifecycle.Has(s) {
			t.Errorf("BuildLifecycle missing state %q", s)
		}
	}
	in := lifecycle.NewInstance(BuildLifecycle)
	in.Reach("success")
	for _, s := range []lifecycle.State{"scheduled", "running", "success"} {
		if !in.Reached(s) {
			t.Errorf("Reach(success) did not imply %s", s)
		}
	}
	if in.Reached("failure") {
		t.Error("Reach(success) leaked into failure terminal")
	}
}

// HCL-surface state lists must NOT include terminal-failure (failed/
// failure) — those are paired-with internally by alpha for waiter
// deadlock avoidance, never referenced by users. If a future change
// adds them, a downstream Alphasfile that wrote `toolchain.X.failed`
// would suddenly become valid — and any service depending on it
// would never bring up. Keep them quarantined.
func TestBarrierStates_excludeTerminalFailure(t *testing.T) {
	for _, s := range ToolchainBarrierStates {
		if s == ToolchainTerminalFailure {
			t.Errorf("ToolchainBarrierStates leaks terminal failure %q to HCL", s)
		}
	}
	for _, s := range BuildBarrierStates {
		if s == BuildTerminalFailure {
			t.Errorf("BuildBarrierStates leaks terminal failure %q to HCL", s)
		}
	}
	for _, s := range ServiceBarrierStates {
		if s == ServiceTerminalFailure {
			t.Errorf("ServiceBarrierStates leaks terminal failure %q to HCL", s)
		}
	}
}
