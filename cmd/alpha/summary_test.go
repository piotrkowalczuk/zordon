package main

import (
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

// buildStartSummary is the adapter between the live serviceCtx/provisionCtx
// timing and summary.Build. Its own logic — include only services that
// reached ready, map the runtime.after deps and provision parents, order by
// when each service started — is what this covers. The exact phase math is
// summary.Build's, tested there; here start order is pinned via explicit
// startedAt values so it doesn't depend on the real clock.
func TestBuildStartSummary(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	svc := func(name string, after ...string) *alphasfile.Service {
		return &alphasfile.Service{
			Toolchain: "go",
			Runtime:   &alphasfile.RuntimeConfig{Name: name, After: after},
		}
	}
	db := svc("db")
	api := svc("api", "service.go.db@ready")
	stuck := svc("stuck")

	dbCtx := newServiceCtx("db", "go")
	dbCtx.startedAt = base
	dbCtx.build.Reach("success")
	dbCtx.lifecycle.Reach("ready")

	apiCtx := newServiceCtx("api", "go")
	apiCtx.startedAt = base.Add(time.Second) // started after db → sorts second
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
	if got.Services[0].After != nil {
		t.Errorf("db.After = %v, want nil", got.Services[0].After)
	}
	if a := got.Services[1].After; len(a) != 1 || a[0] != "service.go.db@ready" {
		t.Errorf("api.After = %v, want [service.go.db@ready]", a)
	}

	if len(got.Provisions) != 1 {
		t.Fatalf("summary has %d provisions, want 1", len(got.Provisions))
	}
	if p := got.Provisions[0]; p.Name != "seed" || p.Service != "api" || p.Detached {
		t.Errorf("provision = {name %q service %q detached %v}, want {seed api false}", p.Name, p.Service, p.Detached)
	}
}
