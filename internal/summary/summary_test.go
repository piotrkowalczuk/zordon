package summary

import (
	"testing"
	"time"
)

func TestBuild_phasesAndOrder(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	at := func(ms int64) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

	// Passed out of start order (api before db) to prove Build sorts by
	// Started; api's wait is db's whole readiness (it depends on db).
	services := []Service{
		{
			Name: "api", Toolchain: "go",
			Deps: []Dep{
				{Ref: "toolchain.go@ready", SatisfiedAt: at(50)},
				{Ref: "service.go.db.runtime@ready", SatisfiedAt: at(1800)},
			},
			Scheduled: at(0), Started: at(1800), BuildDone: at(2000), Running: at(2020), Ready: at(2400),
		},
		{
			Name: "db", Toolchain: "go",
			Deps: []Dep{
				{Ref: "toolchain.go@ready", SatisfiedAt: at(0)},
			},
			Scheduled: at(0), Started: at(0), BuildDone: at(1400), Running: at(1410), Ready: at(1800),
		},
	}
	provisions := []Provision{
		{
			Name: "seed", Service: "api", After: []string{"service.go.db@ready"},
			Scheduled: at(1810), Running: at(1820), Done: at(2920),
		},
		{
			Name: "warmup", Service: "api", Detached: true,
			Scheduled: at(2400), Running: at(2405), // Done zero → still running
		},
	}

	got := Build(at(0), at(3000), services, provisions)

	if got.TotalMS != 3000 {
		t.Errorf("TotalMS = %d, want 3000", got.TotalMS)
	}

	if len(got.Services) != 2 || got.Services[0].Name != "db" || got.Services[1].Name != "api" {
		t.Fatalf("services not in start order (db, api): %+v", got.Services)
	}

	api := got.Services[1]
	for _, c := range []struct {
		label     string
		got, want int64
	}{
		{"wait", api.WaitMS, 1800},
		{"build", api.BuildMS, 200},
		{"spawn", api.SpawnMS, 20},
		{"ready", api.ReadyMS, 380},
		{"total", api.TotalMS, 2400},
	} {
		if c.got != c.want {
			t.Errorf("api %s = %d, want %d", c.label, c.got, c.want)
		}
	}
	// api's deps sorted by ascending wait: toolchain (50ms) then db
	// (1800ms), with db — the last to clear — flagged the long pole.
	if len(api.Deps) != 2 {
		t.Fatalf("api.Deps = %+v, want 2", api.Deps)
	}
	if d := api.Deps[0]; d.Ref != "toolchain.go@ready" || d.WaitMS != 50 || d.LongPole {
		t.Errorf("api.Deps[0] = %+v, want toolchain 50ms not-longpole", d)
	}
	if d := api.Deps[1]; d.Ref != "service.go.db.runtime@ready" || d.WaitMS != 1800 || !d.LongPole {
		t.Errorf("api.Deps[1] = %+v, want db 1800ms longpole", d)
	}
	// db has a single dep → not flagged (nothing to disambiguate).
	if db := got.Services[0]; len(db.Deps) != 1 || db.Deps[0].LongPole {
		t.Errorf("db.Deps = %+v, want 1 dep, not long pole", db.Deps)
	}

	if len(got.Provisions) != 2 || got.Provisions[0].Name != "seed" || got.Provisions[1].Name != "warmup" {
		t.Fatalf("provisions not in run order (seed, warmup): %+v", got.Provisions)
	}
	if seed := got.Provisions[0]; seed.WaitMS != 10 || seed.RunMS != 1100 || seed.Service != "api" {
		t.Errorf("seed = {wait %d run %d service %q}, want {10 1100 api}", seed.WaitMS, seed.RunMS, seed.Service)
	}
	if warmup := got.Provisions[1]; !warmup.Detached || warmup.RunMS != 0 {
		t.Errorf("warmup = {detached %v run %d}, want detached with run 0", warmup.Detached, warmup.RunMS)
	}
}

func TestBuild_clampsBadDeltasAndPreservesInput(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	at := func(ms int64) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

	services := []Service{
		{Name: "z", Scheduled: at(100), Started: at(50)}, // Started before Scheduled
		{Name: "a", Scheduled: at(0), Started: at(10)},
	}
	orig := append([]Service(nil), services...)

	got := Build(at(0), at(200), services, nil)

	var z ServiceTiming
	for _, s := range got.Services {
		if s.Name == "z" {
			z = s
		}
	}
	if z.WaitMS != 0 {
		t.Errorf("z.WaitMS = %d, want 0 (non-monotonic delta clamped)", z.WaitMS)
	}
	// A missing endpoint (Ready never set) yields zero, not garbage.
	if z.TotalMS != 0 || z.ReadyMS != 0 {
		t.Errorf("z total=%d ready=%d, want 0/0 for missing endpoints", z.TotalMS, z.ReadyMS)
	}
	// Build clones before sorting: the caller's slice order is untouched.
	if services[0].Name != orig[0].Name || services[1].Name != orig[1].Name {
		t.Errorf("Build mutated input order: got %q,%q", services[0].Name, services[1].Name)
	}
}
