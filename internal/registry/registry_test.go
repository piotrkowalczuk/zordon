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
)

func TestRegister_thenLookup(t *testing.T) {
	home := t.TempDir()
	want := Entry{Port: 51010, FsHash: "abc", Service: "svc", PGID: 9347}

	if err := Register(home, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LookupPort(home, 51010)
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
func TestRegister_sameServiceReplacesPriorEntry(t *testing.T) {
	home := t.TempDir()
	_ = Register(home, Entry{Port: 1000, FsHash: "abc", Service: "svc", PGID: 100})
	_ = Register(home, Entry{Port: 2000, FsHash: "abc", Service: "svc", PGID: 200})

	got, ok, _ := LookupPort(home, 2000)
	if !ok || got.PGID != 200 {
		t.Errorf("new entry not present: ok=%v pgid=%d", ok, got.PGID)
	}
	if _, ok, _ := LookupPort(home, 1000); ok {
		t.Error("stale entry on port 1000 survived a re-register")
	}
}

func TestListByFsHash_filtersByFsHash(t *testing.T) {
	home := t.TempDir()
	_ = Register(home, Entry{Port: 1, FsHash: "abc", Service: "s1", PGID: 1})
	_ = Register(home, Entry{Port: 2, FsHash: "xyz", Service: "s2", PGID: 2})
	_ = Register(home, Entry{Port: 3, FsHash: "abc", Service: "s3", PGID: 3})

	got, err := ListByFsHash(home, "abc")
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

func TestRemove_byFsHashAndService(t *testing.T) {
	home := t.TempDir()
	_ = Register(home, Entry{Port: 1, FsHash: "abc", Service: "keep", PGID: 1})
	_ = Register(home, Entry{Port: 2, FsHash: "abc", Service: "drop", PGID: 2})

	if err := Remove(home, "abc", "drop"); err != nil {
		t.Fatal(err)
	}
	got, _ := ListByFsHash(home, "abc")
	if len(got) != 1 || got[0].Service != "keep" {
		t.Errorf("Remove left wrong entries: %+v", got)
	}
}

func TestRemove_missingIsNoOp(t *testing.T) {
	home := t.TempDir()
	if err := Remove(home, "doesnt", "exist"); err != nil {
		t.Errorf("Remove on absent entry errored: %v", err)
	}
}

// REGRESSION: the registry file must be safe to read on first run
// (file doesn't exist yet). LookupPort/ListByFsHash should return
// "no entries" without surfacing an os.IsNotExist error.
func TestLookup_emptyHomeReturnsNoEntries(t *testing.T) {
	home := t.TempDir()

	if _, ok, err := LookupPort(home, 1000); err != nil || ok {
		t.Errorf("LookupPort on fresh home: ok=%v err=%v; want ok=false err=nil", ok, err)
	}
	got, err := ListByFsHash(home, "anything")
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
func TestRegister_concurrentWritersSerialize(t *testing.T) {
	home := t.TempDir()

	const goroutines = 8
	const each = 5
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range each {
				err := Register(home, Entry{
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

	got, err := ListByFsHash(home, "fs")
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
func TestReapByFsHash_dropsDeadEntriesWithoutSignal(t *testing.T) {
	home := t.TempDir()
	// PGID well past typical live PID space: kill(-pgid, 0) returns
	// ESRCH; reaper should skip the signal but still drop the row.
	_ = Register(home, Entry{Port: 1, FsHash: "abc", Service: "ghost", PGID: 999999})
	_ = Register(home, Entry{Port: 2, FsHash: "xyz", Service: "other", PGID: 999998})

	if err := ReapByFsHash(home, "abc", 10*time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := ListByFsHash(home, "abc"); len(got) != 0 {
		t.Errorf("dead entries not pruned: %+v", got)
	}
	if got, _ := ListByFsHash(home, "xyz"); len(got) != 1 {
		t.Errorf("foreign fs_hash entry collateral-damaged: %+v", got)
	}
}

// REGRESSION: reaper sends signals only to entries matching fsHash;
// peers from other federation levels / projects on the same host
// must not get killed.
func TestReapByFsHash_sparesOtherFsHashes(t *testing.T) {
	home := t.TempDir()

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

	_ = Register(home, Entry{Port: 1, FsHash: "alive", Service: "sleeper", PGID: pgid})
	_ = Register(home, Entry{Port: 2, FsHash: "abc", Service: "ghost", PGID: 999999})

	if err := ReapByFsHash(home, "abc", 10*time.Millisecond, nil); err != nil {
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
	_ = Register(home, Entry{Port: 1, FsHash: "abc", Service: "svc", PGID: 1})

	data, err := os.ReadFile(filepath.Join(home, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[0] != '[' {
		t.Errorf("registry.json doesn't look like a JSON array: %q", string(data))
	}
}
