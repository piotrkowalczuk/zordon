package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlock"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
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

func TestWatchBringupProgress_reportsPendingPhasesInSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc := func(name string) *alphasfile.Service {
			return &alphasfile.Service{
				Toolchain: "go",
				Runtime:   &alphasfile.RuntimeConfig{Name: name},
			}
		}
		db := svc("db")
		api := svc("api")

		dbCtx := newServiceCtx("db", "go")
		dbCtx.build.Reach("running")
		apiCtx := newServiceCtx("api", "go")
		seed := newProvisionCtx("service.go.api", &alphasfile.ProvisionStep{Name: "seed"})

		services := []*alphasfile.Service{db, api}
		ctxs := map[string]*serviceCtx{"db": dbCtx, "api": apiCtx}
		provs := []provEntry{{pc: seed, parent: apiCtx, detached: false}}

		var logBuf, streamBuf bytes.Buffer
		log := zlog.New(&logBuf, true)
		stream := newSafeEncoder(protocol.NewEncoder(&streamBuf))
		state := &alphaState{shutdownCh: make(chan struct{})}

		done := make(chan struct{})
		go watchBringupProgress(done, time.Now(), services, ctxs, provs, state, stream, log)

		select {
		case <-time.After(5 * time.Second):
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
		synctest.Wait()
		dbCtx.build.Reach("success")
		dbCtx.lifecycle.Reach("ready")
		apiCtx.build.Reach("success")
		apiCtx.lifecycle.Reach("running")
		seed.lifecycle.Reach("running")
		synctest.Wait()

		select {
		case <-time.After(5 * time.Second):
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
		synctest.Wait()
		close(done)
		apiCtx.lifecycle.Reach("ready")
		seed.lifecycle.Reach("success")
		synctest.Wait()

		gotLog := logBuf.String()
		if !strings.Contains(gotLog, "still bringing up (5s elapsed): waiting on db (building), api (waiting for dependencies), provision api/seed (waiting for dependencies)") {
			t.Fatalf("log missing 5s progress report:\n%s", gotLog)
		}
		if !strings.Contains(gotLog, "still bringing up (10s elapsed): waiting on api (probing readiness), provision api/seed (running)") {
			t.Fatalf("log missing 10s progress report:\n%s", gotLog)
		}
		if !strings.Contains(streamBuf.String(), "still bringing up (10s elapsed)") {
			t.Fatalf("stream missing streamed EventLog progress:\n%s", streamBuf.String())
		}
	})
}

func TestAlphaState_shutdownCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state := &alphaState{shutdownCh: make(chan struct{})}
		ctx1 := state.shutdownCtx()
		ctx2 := state.shutdownCtx()
		if ctx1 != ctx2 {
			t.Fatal("shutdownCtx must return the same alpha-scoped context across calls")
		}
		if ctx1.Err() != nil {
			t.Fatalf("shutdownCtx canceled before requestShutdown: %v", ctx1.Err())
		}
		state.requestShutdown("test shutdown")
		synctest.Wait()
		if ctx1.Err() == nil {
			t.Fatal("shutdownCtx not canceled after requestShutdown")
		}
	})
}

func TestAlphaState_shutdownAll_ownerAwareFileCleanup(t *testing.T) {
	dir := t.TempDir()
	ownedPath := filepath.Join(dir, "owned.env")
	replacedPath := filepath.Join(dir, "replaced.env")
	modifiedPath := filepath.Join(dir, "modified.env")

	state := &alphaState{shutdownCh: make(chan struct{})}
	var logBuf bytes.Buffer
	log := zlog.New(&logBuf, true)

	err := materializeFiles([]*alphasfile.File{
		{Name: "owned", Path: ownedPath, Body: "OWNED=1\n"},
		{Name: "replaced", Path: replacedPath, Body: "OLD=1\n"},
		{Name: "modified", Path: modifiedPath, Body: "ORIG=1\n"},
	}, state, log)
	if err != nil {
		t.Fatalf("materializeFiles: %v", err)
	}

	// Simulate a newer alpha replacing replacedPath atomically (new inode).
	if err := zfs.AtomicWrite(replacedPath, []byte("NEW=2\n")); err != nil {
		t.Fatalf("AtomicWrite replacedPath: %v", err)
	}

	// Simulate an in-place edit on the same inode for modifiedPath.
	if err := os.WriteFile(modifiedPath, []byte("EDITED=2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile modifiedPath: %v", err)
	}

	state.shutdownAll(log)

	if zfs.Exists(ownedPath) {
		t.Errorf("owned file %s still exists after shutdownAll; want removed", ownedPath)
	}
	gotReplaced, err := zfs.Read(replacedPath)
	if err != nil || string(gotReplaced) != "NEW=2\n" {
		t.Errorf("replaced file = (%q, %v); want (\"NEW=2\\n\", nil)", string(gotReplaced), err)
	}
	gotModified, err := zfs.Read(modifiedPath)
	if err != nil || string(gotModified) != "EDITED=2\n" {
		t.Errorf("modified file = (%q, %v); want (\"EDITED=2\\n\", nil)", string(gotModified), err)
	}
}

func TestAlphaState_concurrentCreateAndDestroy_consistentFileSideEffect(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	log := zlog.New(io.Discard, true)

	const generations = 16
	states := make([]*alphaState, generations)
	svcRelease := make([]chan struct{}, generations)

	// Create generations 0..generations-2 and start their shutdownAll in
	// parallel while their supervised services are still draining.
	var teardownWG sync.WaitGroup
	for i := range generations - 1 {
		st := &alphaState{shutdownCh: make(chan struct{})}
		sc := newServiceCtx(fmt.Sprintf("svc-%d", i), "go")
		st.addService(sc)
		body := fmt.Sprintf("GEN=%d\n", i)
		if err := materializeFiles([]*alphasfile.File{{Name: "dotenv", Path: envPath, Body: body}}, st, log); err != nil {
			t.Fatalf("materializeFiles gen %d: %v", i, err)
		}
		states[i] = st
		ch := make(chan struct{})
		svcRelease[i] = ch
		go func(sc *serviceCtx, release <-chan struct{}) {
			<-sc.stopCh
			<-release
			close(sc.done)
		}(sc, ch)

		teardownWG.Add(1)
		go func(st *alphaState) {
			defer teardownWG.Done()
			st.shutdownAll(log)
		}(st)
	}

	// Create the final surviving alpha while all prior generations are mid-teardown.
	finalState := &alphaState{shutdownCh: make(chan struct{})}
	wantFinal := fmt.Sprintf("GEN=%d\n", generations-1)
	if err := materializeFiles([]*alphasfile.File{{Name: "dotenv", Path: envPath, Body: wantFinal}}, finalState, log); err != nil {
		t.Fatalf("materializeFiles final: %v", err)
	}

	// Unblock all prior generations' service exits concurrently so their file
	// cleanup races against each other and against the final generation's file.
	for i := range generations - 1 {
		close(svcRelease[i])
	}
	teardownWG.Wait()

	got, err := zfs.Read(envPath)
	if err != nil {
		t.Fatalf(".env missing after concurrent teardowns of older generations: %v", err)
	}
	if string(got) != wantFinal {
		t.Fatalf(".env content = %q, want %q", string(got), wantFinal)
	}

	// Also verify serialized create/destroy cycles through WorkspaceKeychain.LockAlpha.
	wsKC := zlock.NewGlobalKeychain(dir).ForWorkspace(dir)
	var cycleWG sync.WaitGroup
	for i := range 8 {
		cycleWG.Add(1)
		go func(id int) {
			defer cycleWG.Done()
			if err := wsKC.WaitUnlockedAlpha(t.Context()); err != nil {
				t.Errorf("WaitUnlockedAlpha(%d): %v", id, err)
				return
			}
			unlock, err := wsKC.LockAlpha(t.Context())
			if err != nil {
				t.Errorf("LockAlpha(%d): %v", id, err)
				return
			}
			defer unlock()

			st := &alphaState{shutdownCh: make(chan struct{})}
			body := fmt.Sprintf("LOCKED_GEN=%d\n", id)
			if err := materializeFiles([]*alphasfile.File{{Name: "dotenv", Path: envPath, Body: body}}, st, log); err != nil {
				t.Errorf("materializeFiles(%d): %v", id, err)
				return
			}
			gotInLock, err := zfs.Read(envPath)
			if err != nil || string(gotInLock) != body {
				t.Errorf("inside LockAlpha(%d): .env = (%q, %v), want %q", id, string(gotInLock), err, body)
			}
			st.shutdownAll(log)
			if zfs.Exists(envPath) {
				t.Errorf("inside LockAlpha(%d): .env still exists after shutdownAll", id)
			}
		}(i)
	}
	cycleWG.Wait()

	if zfs.Exists(envPath) {
		t.Fatalf(".env still exists after all locked alpha lifecycles destroyed")
	}
}
