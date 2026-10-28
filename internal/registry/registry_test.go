package registry

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestRegistry_Register_thenLookup(t *testing.T) {
	reg := New(zfs.StateDir(t.TempDir()))
	want := Entry{Port: 51010, FsHash: "abc", Service: "svc", PGID: 9347}

	if err := reg.Register(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := reg.LookupPort(t.Context(), 51010)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("LookupPort: entry missing after Register")
	}
	if got.FsHash != want.FsHash || got.Service != want.Service || got.PGID != want.PGID {
		t.Errorf("LookupPort got %+v; want match for %+v", got, want)
	}
}

// REGRESSION: a service that restarts inside the same alpha (reconfigure)
// must overwrite its old row — otherwise stale (PGID, port) entries
// accumulate and confuse later reapers / debuggers.
func TestRegistry_Register_sameServiceReplacesPriorEntry(t *testing.T) {
	reg := New(zfs.StateDir(t.TempDir()))
	_ = reg.Register(t.Context(), Entry{Port: 1000, FsHash: "abc", Service: "svc", PGID: 100})
	_ = reg.Register(t.Context(), Entry{Port: 2000, FsHash: "abc", Service: "svc", PGID: 200})

	got, ok, _ := reg.LookupPort(t.Context(), 2000)
	if !ok || got.PGID != 200 {
		t.Errorf("new entry not present: ok=%v pgid=%d", ok, got.PGID)
	}
	if _, ok, _ := reg.LookupPort(t.Context(), 1000); ok {
		t.Error("stale entry on port 1000 survived a re-register")
	}
}

func TestRegistry_ListByFsHash_filtersByFsHash(t *testing.T) {
	reg := New(zfs.StateDir(t.TempDir()))
	_ = reg.Register(t.Context(), Entry{Port: 1, FsHash: "abc", Service: "s1", PGID: 1})
	_ = reg.Register(t.Context(), Entry{Port: 2, FsHash: "xyz", Service: "s2", PGID: 2})
	_ = reg.Register(t.Context(), Entry{Port: 3, FsHash: "abc", Service: "s3", PGID: 3})

	got, err := reg.ListByFsHash(t.Context(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByFsHash(abc) = %d entries; want 2", len(got))
	}
	for _, e := range got {
		if e.FsHash != "abc" {
			t.Errorf("foreign entry leaked: %+v", e)
		}
	}
}

func TestRegistry_Remove_byFsHashAndService(t *testing.T) {
	reg := New(zfs.StateDir(t.TempDir()))
	_ = reg.Register(t.Context(), Entry{Port: 1, FsHash: "abc", Service: "keep", PGID: 1})
	_ = reg.Register(t.Context(), Entry{Port: 2, FsHash: "abc", Service: "drop", PGID: 2})

	if err := reg.Remove(t.Context(), "abc", "drop"); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.ListByFsHash(t.Context(), "abc")
	if len(got) != 1 || got[0].Service != "keep" {
		t.Errorf("Remove left wrong entries: %+v", got)
	}
}

func TestRegistry_Remove_missingIsNoOp(t *testing.T) {
	reg := New(zfs.StateDir(t.TempDir()))
	if err := reg.Remove(t.Context(), "doesnt", "exist"); err != nil {
		t.Errorf("Remove on absent entry errored: %v", err)
	}
}

// REGRESSION: the registry file must be safe to read on first run
// (file doesn't exist yet). LookupPort/ListByFsHash should return
// "no entries" without surfacing an os.IsNotExist error.
func TestRegistry_Lookup_emptyHomeReturnsNoEntries(t *testing.T) {
	reg := New(zfs.StateDir(t.TempDir()))

	if _, ok, err := reg.LookupPort(t.Context(), 1000); err != nil || ok {
		t.Errorf("LookupPort on fresh home: ok=%v err=%v; want ok=false err=nil", ok, err)
	}
	got, err := reg.ListByFsHash(t.Context(), "anything")
	if err != nil {
		t.Errorf("ListByFsHash on fresh home: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListByFsHash on fresh home returned %d entries", len(got))
	}
}

// REGRESSION: two concurrent writers must serialize via the flock so
// neither sees the other mid-write. Without the lock, json.Unmarshal
// of a half-written file would error and entries would silently vanish.
func TestRegistry_Register_concurrentWritersSerialize(t *testing.T) {
	reg := New(zfs.StateDir(t.TempDir()))

	const goroutines = 8
	const each = 5
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range each {
				err := reg.Register(t.Context(), Entry{
					Port:    g*100 + i,
					FsHash:  "fs",
					Service: fmt.Sprintf("svc-%d-%d", g, i),
					PGID:    g*1000 + i,
				})
				if err != nil {
					t.Errorf("Register: %v", err)
				}
			}
		})
	}
	wg.Wait()

	got, err := reg.ListByFsHash(t.Context(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != goroutines*each {
		t.Errorf("concurrent Register lost rows: got %d, want %d", len(got), goroutines*each)
	}
}

// REGRESSION: reaper must drop entries whose process group is already
// gone (clean exit between Register and Reap) without signalling them,
// AND must drop them from the registry too — otherwise every subsequent
// Reap re-checks them forever.
func TestRegistry_ReapByFsHash_dropsDeadEntriesWithoutSignal(t *testing.T) {
	reg := New(zfs.StateDir(t.TempDir()))
	// PGID well past typical live PID space: kill(-pgid, 0) returns
	// ESRCH; reaper should skip the signal but still drop the row.
	_ = reg.Register(t.Context(), Entry{Port: 1, FsHash: "abc", Service: "ghost", PGID: 999999})
	_ = reg.Register(t.Context(), Entry{Port: 2, FsHash: "xyz", Service: "other", PGID: 999998})

	if err := reg.ReapByFsHash(t.Context(), "abc", 10*time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := reg.ListByFsHash(t.Context(), "abc"); len(got) != 0 {
		t.Errorf("dead entries not pruned: %+v", got)
	}
	if got, _ := reg.ListByFsHash(t.Context(), "xyz"); len(got) != 1 {
		t.Errorf("foreign fs_hash entry collateral-damaged: %+v", got)
	}
}

// REGRESSION: reaper sends signals only to entries matching fsHash;
// peers from other federation levels / projects on the same host
// must not get killed.
func TestRegistry_ReapByFsHash_sparesOtherFsHashes(t *testing.T) {
	reg := New(zfs.StateDir(t.TempDir()))

	// Spawn a sleeping child with its own pgid (Setpgid: true). The
	// reap below targets fs_hash "abc"; our sleeper is registered as
	// "alive" and MUST survive.
	cmdSleep := exec.Command("sleep", "60")
	cmdSleep.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmdSleep.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmdSleep.Process.Pid, syscall.SIGKILL)
		_, _ = cmdSleep.Process.Wait()
	})
	pgid := cmdSleep.Process.Pid // Setpgid = true ⇒ pid == pgid

	_ = reg.Register(t.Context(), Entry{Port: 1, FsHash: "alive", Service: "sleeper", PGID: pgid})
	_ = reg.Register(t.Context(), Entry{Port: 2, FsHash: "abc", Service: "ghost", PGID: 999999})

	if err := reg.ReapByFsHash(t.Context(), "abc", 10*time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Errorf("reap targeting 'abc' killed sleeper from 'alive' fs_hash: %v", err)
	}
}

// Sanity: registry.json must be valid JSON post-write so external
// tooling (jq, future `zordon ports` debug cmd) can parse it.
func TestWriteEntries_producesParseableJSON(t *testing.T) {
	home := t.TempDir()
	reg := New(zfs.StateDir(home))
	_ = reg.Register(t.Context(), Entry{Port: 1, FsHash: "abc", Service: "svc", PGID: 1})

	data, err := os.ReadFile(filepath.Join(home, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[0] != '[' {
		t.Errorf("registry.json doesn't look like a JSON array: %q", string(data))
	}
}
