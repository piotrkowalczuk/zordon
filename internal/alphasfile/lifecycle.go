package alphasfile

import "github.com/piotrkowalczuk/zordon/internal/lifecycle"

// ServiceLifecycle is the precedence DAG for a managed service.
//
//	scheduled → running → ready    (success path, "operational")
//	                   → failed    (bringup couldn't reach ready)
//	ready → stopped                (graceful exit after operation)
//
// `done` is intentionally NOT a lifecycle node — it's composed in
// alpha via barrier.Any(ready, failed, stopped) per instance, because
// having it in the DAG would force a single set of predecessors that
// can't represent "success OR failure".
var ServiceLifecycle = lifecycle.NewDef(
	[]lifecycle.State{"scheduled", "running", "ready", "failed", "stopped"},
	[]lifecycle.Edge{
		{From: "scheduled", To: "running"},
		{From: "running", To: "ready"},
		{From: "running", To: "failed"},
		{From: "ready", To: "stopped"},
	},
)

// ProvisionLifecycle is the precedence DAG for a one-shot provision
// step.
//
//	scheduled → running → success    (cmd exited 0, verify passed)
//	                   → failure     (cmd non-zero or dep failed)
//
// Same shape as service but with success/failure terminals instead of
// ready/failed/stopped; provisions are transient tasks, not daemons.
var ProvisionLifecycle = lifecycle.NewDef(
	[]lifecycle.State{"scheduled", "running", "success", "failure"},
	[]lifecycle.Edge{
		{From: "scheduled", To: "running"},
		{From: "running", To: "success"},
		{From: "running", To: "failure"},
	},
)

// Terminal-failure states per entity type. Alpha pairs every waiter on
// any service/provision barrier with a select on this state's barrier,
// so a dep that lands in terminal failure unblocks waiters instead of
// leaving them deadlocked. Not exposed in HCL — pairing is automatic.
const (
	ServiceTerminalFailure   lifecycle.State = "failed"
	ProvisionTerminalFailure lifecycle.State = "failure"
)

// ServiceBarrierStates is the set of states a user may reference via
// HCL (e.g. service.go.api.ready). "done" is a synthetic composed
// barrier exposed alongside the lifecycle states. "failed" is NOT
// exposed in HCL on purpose — run-on-failure is a future feature; for
// now alpha uses the failure barrier only for waiter-pairing.
var ServiceBarrierStates = []lifecycle.State{
	"scheduled", "running", "ready", "stopped", "done",
}

// ProvisionBarrierStates: HCL surface for provision barriers. "failure"
// is not exposed for the same reason as service.failed.
var ProvisionBarrierStates = []lifecycle.State{
	"scheduled", "running", "success", "done",
}

// SyntheticStateDone is the name of the composed "outcome decided"
// barrier that lives outside the precedence DAG. Alpha constructs it
// per instance via barrier.Any over terminal states.
const SyntheticStateDone lifecycle.State = "done"
