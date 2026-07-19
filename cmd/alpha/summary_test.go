package main

import (
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

// buildStartSummary is the adapter between the live serviceCtx/provisionCtx
// timing and summary.Build. Its own logic — include only services that
// reached ready, carry each dependency's satisfaction time, order by when
// each service started — is what this covers. Phase math and long-pole
// selection belong to summary.Build (tested there); here start order and the
// dep list are pinned via explicit timestamps so they don't depend on the
// real clock.
func TestBuildStartSummary(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	svc := func(name string) *alphasfile.Service {
		return &alphasfile.Service{
			Toolchain: "go",
			Runtime:   &alphasfile.RuntimeConfig{Name: name},
		}
	}
	db := svc("db")
	api := svc("api")
	stuck := svc("stuck")

	dbCtx := newServiceCtx("db", "go")
	dbCtx.startedAt = base
	dbCtx.deps = []depSat{{ref: "toolchain.go@ready", at: base}}
	dbCtx.build.Reach("success")
	dbCtx.lifecycle.Reach("ready")

	apiCtx := newServiceCtx("api", "go")
	apiCtx.startedAt = base.Add(time.Second) // started after db → sorts second
	apiCtx.deps = []depSat{
		{ref: "toolchain.go@ready", at: base},
		{ref: "service.go.db.runtime@ready", at: base.Add(time.Second)},
	}
	apiCtx.build.Reach("success")
	apiCtx.lifecycle.Reach("ready")

	// stuck reached running but never ready → excluded from the summary.
	stuckCtx := newServiceCtx("stuck", "go")
	stuckCtx.startedAt = base.Add(500 * time.Millisecond)
	stuckCtx.lifecycle.Reach("running")

	seed := newProvisionCtx("service.go.api", &alphasfile.ProvisionStep{
		Name:  "seed",
		After: []string{"service.go.db@ready"},
	})
	seed.lifecycle.Reach("running")
	seed.lifecycle.Reach("success")

	// Services passed out of start order on purpose.
	services := []*alphasfile.Service{api, stuck, db}
	ctxs := map[string]*serviceCtx{"db": dbCtx, "api": apiCtx, "stuck": stuckCtx}
	provs := []provEntry{{pc: seed, parent: apiCtx, detached: false}}

	got := buildStartSummary(services, ctxs, provs, base, base.Add(3*time.Second))

	if len(got.Services) != 2 {
		t.Fatalf("summary has %d services, want 2 (stuck excluded)", len(got.Services))
	}
	if got.Services[0].Name != "db" || got.Services[1].Name != "api" {
		t.Fatalf("services not in start order: got %q, %q; want db, api",
			got.Services[0].Name, got.Services[1].Name)
	}
	if got.Services[0].Toolchain != "go" {
		t.Errorf("db toolchain = %q, want go", got.Services[0].Toolchain)
	}

	// api's two deps carried through; the db barrier cleared last → long pole.
	apiDeps := got.Services[1].Deps
	if len(apiDeps) != 2 {
		t.Fatalf("api.Deps = %+v, want 2", apiDeps)
	}
	if d := apiDeps[len(apiDeps)-1]; d.Ref != "service.go.db.runtime@ready" || !d.LongPole {
		t.Errorf("api long-pole dep = %+v, want service.go.db.runtime@ready flagged", d)
	}

	if len(got.Provisions) != 1 {
		t.Fatalf("summary has %d provisions, want 1", len(got.Provisions))
	}
	if p := got.Provisions[0]; p.Name != "seed" || p.Service != "api" || p.Detached {
		t.Errorf("provision = {name %q service %q detached %v}, want {seed api false}", p.Name, p.Service, p.Detached)
	}
}
