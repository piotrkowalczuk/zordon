package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zenv"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

func TestDescribeBringupWait_reportsWaitingForBuildSlot(t *testing.T) {
	start := time.Now()
	state := &alphaState{shutdownCh: make(chan struct{})}

	svc1 := &alphasfile.Service{Toolchain: "go", Runtime: &alphasfile.RuntimeConfig{Name: "api"}}
	svc2 := &alphasfile.Service{Toolchain: "rust", Runtime: &alphasfile.RuntimeConfig{Name: "worker"}}

	sc1 := newServiceCtx("api", "go")
	sc1.build.Reach("running")
	sc1.setBuildPhase("building")

	sc2 := newServiceCtx("worker", "rust")
	sc2.build.Reach("running")
	sc2.setBuildPhase("waiting for build slot")

	ctxs := map[string]*serviceCtx{"api": sc1, "worker": sc2}
	got := describeBringupWait([]*alphasfile.Service{svc1, svc2}, ctxs, nil, state, start, start.Add(5*time.Second))
	want := "api (building), worker (waiting for build slot)"
	if got != want {
		t.Fatalf("describeBringupWait = %q, want %q", got, want)
	}
}

func TestPrepareBuild_serializesAndInjectsBuildQuotaEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "svc2-env.txt")
	firstStarted := filepath.Join(dir, "svc1-started")
	releaseFirst := filepath.Join(dir, "svc1-release")

	state := &alphaState{
		zordonHome: dir,
		shutdownCh: make(chan struct{}),
		config:     &alphasfile.Alphasfile{},
	}
	state.buildLimiter.SetMaxCPU(2)

	var logBuf, wireBuf bytes.Buffer
	log := zlog.New(&logBuf, true)
	stream := newSafeEncoder(protocol.NewEncoder(&wireBuf))

	svc1 := &alphasfile.Service{
		Toolchain: "go",
		Package: &alphasfile.Package{
			Src: dir,
			BuildCmd: []string{
				"/bin/sh", "-c",
				"touch '" + firstStarted + "'; while [ ! -f '" + releaseFirst + "' ]; do sleep 0.01; done",
			},
		},
		Runtime: &alphasfile.RuntimeConfig{
			Name: "svc1",
			Dir:  dir,
		},
	}
	svc2 := &alphasfile.Service{
		Toolchain: "rust",
		Package: &alphasfile.Package{
			Src: dir,
			BuildCmd: []string{
				"/bin/sh", "-c",
				"printf 'CARGO=%s\nBUNDLE=%s\nMAKE=%s\nCMAKE=%s\nGO=%s\n' \"$CARGO_BUILD_JOBS\" \"$BUNDLE_JOBS\" \"$MAKEFLAGS\" \"$CMAKE_BUILD_PARALLEL_LEVEL\" \"$GOMAXPROCS\" > '" + envFile + "'",
			},
		},
		Runtime: &alphasfile.RuntimeConfig{
			Name:     "svc2",
			Dir:      dir,
			BuildEnv: zenv.EnvironmentVariables{"CARGO_BUILD_JOBS": "1"},
		},
	}

	sc1 := newServiceCtx("svc1", "go")
	sc2 := newServiceCtx("svc2", "rust")

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if err := prepareBuild(t.Context(), svc1, sc1, "svc1", dir, false, state, stream, log); err != nil {
			t.Errorf("prepareBuild(svc1): %v", err)
		}
	}()

	waitFileExists(t, firstStarted)

	go func() {
		defer wg.Done()
		err := prepareBuild(t.Context(), svc2, sc2, "svc2", dir, false, state, stream, log)
		if err != nil {
			t.Errorf("prepareBuild(svc2): %v", err)
		}
	}()

	waitBuildPhase(t, sc2, "waiting for build slot")
	if _, err := os.Stat(envFile); err == nil {
		t.Fatal("svc2 ran its build while svc1 still held the build slot")
	}

	if err := os.WriteFile(releaseFirst, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read svc2 env file: %v", err)
	}
	gotEnv := string(raw)
	for _, want := range []string{"CARGO=1", "BUNDLE=2", "MAKE=-j2", "CMAKE=2", "GO=2"} {
		if !strings.Contains(gotEnv, want) {
			t.Errorf("svc2 build env missing %q in:\n%s", want, gotEnv)
		}
	}
	if !strings.Contains(logBuf.String(), "prepare svc2: waiting for build slot") {
		t.Errorf("alpha log missing build slot wait message:\n%s", logBuf.String())
	}
}

func waitFileExists(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitBuildPhase(t *testing.T, sc *serviceCtx, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sc.BuildPhase() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for BuildPhase %q (got %q)", want, sc.BuildPhase())
}
