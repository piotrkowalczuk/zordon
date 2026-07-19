// Package summary builds the timing report printed after a successful
// `zordon start`: each service's bringup broken into phases (wait / build /
// spawn / ready) and each provision's run duration, ordered by when things
// actually started so the report reads as the real bringup sequence.
//
// The supervisor (alpha) captures raw per-entity timestamps and calls Build;
// the resulting StartSummary travels on the wire (protocol.Event.Summary) and
// the client renders it. Keeping the assembly here — pure, dependency-free —
// makes the phase math and ordering testable without a running supervisor.
package summary

import (
	"cmp"
	"slices"
	"time"
)

// StartSummary is the whole-bringup timing report. Durations are whole
// milliseconds, measured with a monotonic clock.
type StartSummary struct {
	TotalMS    int64             `json:"total_ms"`
	Services   []ServiceTiming   `json:"services,omitempty"`   // observed start order
	Provisions []ProvisionTiming `json:"provisions,omitempty"` // observed run order
}

// ServiceTiming breaks one service's bringup into phases: wait (blocked on
// dependencies) → build (workspace prepare + build cmd) → spawn (exec
// launch) → ready (readiness probe / stabilization). Total is scheduled→ready.
// Deps attributes the wait phase across the individual dependencies.
type ServiceTiming struct {
	Name      string      `json:"name"`
	Toolchain string      `json:"toolchain,omitempty"`
	Deps      []DepTiming `json:"deps,omitempty"`
	WaitMS    int64       `json:"wait_ms"`
	BuildMS   int64       `json:"build_ms"`
	SpawnMS   int64       `json:"spawn_ms"`
	ReadyMS   int64       `json:"ready_ms"`
	TotalMS   int64       `json:"total_ms"`
}

// DepTiming is how long a service was blocked on one dependency — its
// runtime.after refs plus the implicit toolchain. WaitMS is scheduled →
// dependency satisfied. LongPole marks the dep that gated the service's
// start (the last to clear); set only when the service has more than one,
// so a lone dependency isn't labelled the bottleneck trivially.
type DepTiming struct {
	Ref      string `json:"ref"`
	WaitMS   int64  `json:"wait_ms"`
	LongPole bool   `json:"long_pole,omitempty"`
}

// ProvisionTiming reports one provision's run. RunMS is zero (omitted) for a
// detached provision that had not finished when the bringup completed.
type ProvisionTiming struct {
	Name     string   `json:"name"`
	Service  string   `json:"service,omitempty"`
	After    []string `json:"after,omitempty"`
	Detached bool     `json:"detached,omitempty"`
	WaitMS   int64    `json:"wait_ms,omitempty"`
	RunMS    int64    `json:"run_ms,omitempty"`
}

// Service is the raw per-service timing the supervisor captured. A zero-value
// timestamp means "phase boundary not reached"; any span touching it is
// reported as zero.
type Service struct {
	Name      string
	Toolchain string
	Deps      []Dep
	Scheduled time.Time
	Started   time.Time // deps cleared, the service's own work begins
	BuildDone time.Time
	Running   time.Time // process launched
	Ready     time.Time
}

// Dep is one resolved dependency the service waited on and when its barrier
// fired (the true close time, so attribution is correct regardless of the
// order the supervisor happened to observe the barriers in).
type Dep struct {
	Ref         string
	SatisfiedAt time.Time
}

// Provision is the raw per-provision timing the supervisor captured. Done is
// the terminal (success or failure) time, zero if still running (detached).
type Provision struct {
	Name      string
	Service   string
	After     []string
	Detached  bool
	Scheduled time.Time
	Running   time.Time
	Done      time.Time
}

// Build assembles the report. Services are ordered by Started and provisions
// by Running, so the summary reads as the real bringup sequence. Inputs are
// cloned before sorting — the caller's slices are left untouched.
func Build(bringupStart, now time.Time, services []Service, provisions []Provision) *StartSummary {
	s := &StartSummary{TotalMS: msBetween(bringupStart, now)}

	svc := slices.Clone(services)
	slices.SortFunc(svc, func(a, b Service) int { return a.Started.Compare(b.Started) })
	for _, x := range svc {
		s.Services = append(s.Services, ServiceTiming{
			Name:      x.Name,
			Toolchain: x.Toolchain,
			Deps:      depTimings(x.Scheduled, x.Deps),
			WaitMS:    msBetween(x.Scheduled, x.Started),
			BuildMS:   msBetween(x.Started, x.BuildDone),
			SpawnMS:   msBetween(x.BuildDone, x.Running),
			ReadyMS:   msBetween(x.Running, x.Ready),
			TotalMS:   msBetween(x.Scheduled, x.Ready),
		})
	}

	prov := slices.Clone(provisions)
	slices.SortFunc(prov, func(a, b Provision) int { return a.Running.Compare(b.Running) })
	for _, x := range prov {
		s.Provisions = append(s.Provisions, ProvisionTiming{
			Name:     x.Name,
			Service:  x.Service,
			After:    x.After,
			Detached: x.Detached,
			WaitMS:   msBetween(x.Scheduled, x.Running),
			RunMS:    msBetween(x.Running, x.Done),
		})
	}

	return s
}

// depTimings attributes a service's wait phase across its dependencies:
// each dep's WaitMS is scheduled → satisfied. Ordered by ascending wait so
// the bottleneck reads last, and that last (largest-wait) dep is flagged
// LongPole — the one that actually gated the service's start — but only
// when there's more than one dep to disambiguate.
func depTimings(scheduled time.Time, deps []Dep) []DepTiming {
	if len(deps) == 0 {
		return nil
	}
	out := make([]DepTiming, len(deps))
	for i, d := range deps {
		out[i] = DepTiming{Ref: d.Ref, WaitMS: msBetween(scheduled, d.SatisfiedAt)}
	}
	slices.SortFunc(out, func(a, b DepTiming) int { return cmp.Compare(a.WaitMS, b.WaitMS) })
	if len(out) > 1 {
		out[len(out)-1].LongPole = true
	}
	return out
}

// msBetween is b-a in whole milliseconds, clamped to zero for a missing
// endpoint or a non-monotonic delta.
func msBetween(a, b time.Time) int64 {
	if a.IsZero() || b.IsZero() {
		return 0
	}
	if d := b.Sub(a); d > 0 {
		return d.Milliseconds()
	}
	return 0
}
