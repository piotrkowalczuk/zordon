// Package lifecycle declares a precedence DAG of states for an entity
// and exposes a barrier.Barrier per state. Reach(s) triggers s's
// barrier plus every transitive predecessor, so observers waiting on
// an early state get unblocked even when the entity sprinted past it
// before they registered.
//
// Why: independent barriers per state would make the implementer
// responsible for firing every predecessor by hand at the transition
// site — easy to forget, and "fast-path" transitions that skip explicit
// closings would leave stale waiters hanging. Defining the DAG once
// and resolving predecessors at Reach time is both safer and more
// declarative.
package lifecycle

import (
	"time"

	"github.com/piotrkowalczuk/zordon/internal/barrier"
)

// State is the canonical name of a node in the lifecycle. Picked by
// each entity type; lifecycle doesn't prescribe a vocabulary, only the
// shape (DAG, predecessors, monotonic Reach).
type State string

// Edge declares "From must close before (or together with) To" — i.e.
// reaching To implies From has already been reached.
type Edge struct{ From, To State }

// Def is the static lifecycle declaration. Allocate once per entity
// type and share across all instances of that type.
type Def struct {
	states []State
	// preds is the transitive predecessor set per state, precomputed in
	// NewDef so Reach is just a flat range over a slice.
	preds map[State][]State
	set   map[State]struct{}
}

// NewDef compiles the DAG. Edges are interpreted as "From precedes To";
// transitive closure is computed once. Unknown states in edges are
// silently ignored (only what's in states matters).
func NewDef(states []State, edges []Edge) *Def {
	d := &Def{
		states: append([]State(nil), states...),
		preds:  make(map[State][]State, len(states)),
		set:    make(map[State]struct{}, len(states)),
	}
	for _, s := range states {
		d.set[s] = struct{}{}
	}
	// Direct predecessors per state.
	direct := make(map[State][]State, len(states))
	for _, e := range edges {
		if _, ok := d.set[e.From]; !ok {
			continue
		}
		if _, ok := d.set[e.To]; !ok {
			continue
		}
		direct[e.To] = append(direct[e.To], e.From)
	}
	// DFS to compute transitive closure per state.
	for _, s := range states {
		visited := make(map[State]bool)
		var out []State
		var dfs func(State)
		dfs = func(node State) {
			for _, p := range direct[node] {
				if visited[p] {
					continue
				}
				visited[p] = true
				out = append(out, p)
				dfs(p)
			}
		}
		dfs(s)
		d.preds[s] = out
	}
	return d
}

// Has reports whether the state is part of this Def.
func (d *Def) Has(s State) bool {
	_, ok := d.set[s]
	return ok
}

// States returns the declared states in declaration order. The slice
// is a defensive copy — callers may mutate it.
func (d *Def) States() []State {
	return append([]State(nil), d.states...)
}

// Instance is one entity's barrier set, indexed by State. Construct
// with NewInstance; each Instance is independent (allocates its own
// barriers).
type Instance struct {
	def      *Def
	barriers map[State]*barrier.Barrier
}

// NewInstance allocates an Instance with a fresh barrier per state in
// the Def.
func NewInstance(def *Def) *Instance {
	in := &Instance{def: def, barriers: make(map[State]*barrier.Barrier, len(def.states))}
	for _, s := range def.states {
		in.barriers[s] = barrier.New()
	}
	return in
}

// Reach triggers the barrier for state s plus all transitive
// predecessors. Idempotent: each barrier's Trigger is sync.Once'd.
// Calling with an unknown state is a no-op (the entity might have
// been allocated with a different Def — we don't panic, just don't
// trigger).
func (i *Instance) Reach(s State) {
	if _, ok := i.barriers[s]; !ok {
		return
	}
	for _, p := range i.def.preds[s] {
		i.barriers[p].Trigger()
	}
	i.barriers[s].Trigger()
}

// Barrier returns the barrier for state s, or nil if s isn't in the
// Def. Callers compose these via barrier.Any or use them directly.
func (i *Instance) Barrier(s State) *barrier.Barrier {
	return i.barriers[s]
}

// Reached reports whether the state has been triggered. Convenience
// wrapper around Barrier(s).Triggered().
func (i *Instance) Reached(s State) bool {
	b, ok := i.barriers[s]
	return ok && b.Triggered()
}

// ReachedAt returns when state s was reached and whether it has been. A
// state reached only transitively — its barrier triggered as a predecessor
// of a later Reach — carries that trigger's time. Thin wrapper around
// Barrier(s).FiredAt().
func (i *Instance) ReachedAt(s State) (time.Time, bool) {
	b, ok := i.barriers[s]
	if !ok {
		return time.Time{}, false
	}
	return b.FiredAt()
}
