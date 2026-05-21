// Package registry is a host-wide port → (fs_hash, service) directory
// shared by every alpha running on the machine.
//
// Lives at <zordonHome>/registry.json with an adjacent .lock file
// holding a process-wide LOCK_EX during every read/write so two alphas
// (siblings in a federation chain, or two separate projects) can never
// trample each other's entries.
//
// What it solves:
//
//   - "address already in use" sleuthing: lsof is a fine debugging
//     tool but doesn't tell you WHICH alpha (which Alphasfile, which
//     fs_hash, which service name) owns port X. Registry does.
//   - Orphan reaping: when a previous alpha died non-gracefully
//     (SIGKILL, panic, broken pipe under `zordon start | tail`),
//     its service binaries linger and squat ports. The new alpha
//     for the same fs_hash queries the registry, sends SIGTERM →
//     grace → SIGKILL to every still-alive pgid, then removes the
//     dead entries before binding its own sockets.
//
// What it does NOT solve:
//
//   - Services that don't expose a port (build-only, fire-and-forget
//     provisions). Those need a separate pgid-based mechanism.
//   - Port pre-allocation. Registration happens AFTER the service
//     successfully bound the port; if another process is already
//     squatting, the bind fails first and we never register.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Entry is one row in the registry. The (FsHash, Service) pair is the
// natural key — port can collide across fs_hashes legitimately when
// services bind to interfaces (rare but possible), so we don't enforce
// port uniqueness at registry level. Lookup by port is best-effort:
// the first match wins.
type Entry struct {
	Port    int       `json:"port"`
	FsHash  string    `json:"fs_hash"`
	Service string    `json:"service"`
	PGID    int       `json:"pgid"`
	TS      time.Time `json:"ts"`
}

// Logger is the subset of zlog.Logger the registry needs — broken out
// so the package doesn't pull in alpha's logger and can be tested
// without one.
type Logger interface {
	Warn(src, format string, args ...any)
	Error(src, format string, args ...any)
}

// Register adds an entry under flock. Existing entries for the same
// (FsHash, Service) are replaced — a service restart shouldn't leave
// two stale rows.
func Register(zordonHome string, e Entry) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	return withFile(zordonHome, func(entries []Entry) ([]Entry, error) {
		entries = dropByService(entries, e.FsHash, e.Service)
		return append(entries, e), nil
	})
}

// Remove deletes the (FsHash, Service) entry. No-op if absent — a
// service that never registered (no port) calling Remove on shutdown
// is harmless.
func Remove(zordonHome, fsHash, service string) error {
	return withFile(zordonHome, func(entries []Entry) ([]Entry, error) {
		return dropByService(entries, fsHash, service), nil
	})
}

// LookupPort returns the first entry binding the given port. Useful
// for `zordon ports` style debugging: a user staring at "bind: address
// already in use" wants to know WHICH alpha holds 51010.
func LookupPort(zordonHome string, port int) (Entry, bool, error) {
	var found Entry
	var ok bool
	err := withFileReadOnly(zordonHome, func(entries []Entry) {
		for _, e := range entries {
			if e.Port == port {
				found, ok = e, true
				return
			}
		}
	})
	return found, ok, err
}

// ListByFsHash returns every entry registered under the given fs_hash.
// Used by alpha at startup to identify orphans from a previous run
// of the same Alphasfile instance.
func ListByFsHash(zordonHome, fsHash string) ([]Entry, error) {
	var out []Entry
	err := withFileReadOnly(zordonHome, func(entries []Entry) {
		for _, e := range entries {
			if e.FsHash == fsHash {
				out = append(out, e)
			}
		}
	})
	return out, err
}

// ReapByFsHash sends SIGTERM to every still-alive pgid registered
// under fsHash, waits grace, escalates to SIGKILL on stragglers, then
// removes those entries from the registry. Called at alpha startup
// BEFORE binding the control socket so the new alpha doesn't collide
// with squatters from the previous (dead) one.
//
// Entries whose pgid is already gone (process exited cleanly between
// registration and now) are removed without signal — there's nothing
// to reap.
func ReapByFsHash(zordonHome, fsHash string, grace time.Duration, log Logger) error {
	return withFile(zordonHome, func(entries []Entry) ([]Entry, error) {
		var ours, others []Entry
		for _, e := range entries {
			if e.FsHash == fsHash {
				ours = append(ours, e)
			} else {
				others = append(others, e)
			}
		}
		// SIGTERM pass: only groups that still exist.
		var alive []Entry
		for _, e := range ours {
			if syscall.Kill(-e.PGID, 0) != nil {
				continue // process group already gone
			}
			if log != nil {
				log.Warn("registry", "orphan reaper: SIGTERM pgid=%d (%s on port %d)", e.PGID, e.Service, e.Port)
			}
			_ = syscall.Kill(-e.PGID, syscall.SIGTERM)
			alive = append(alive, e)
		}
		if len(alive) > 0 {
			time.Sleep(grace)
			for _, e := range alive {
				if syscall.Kill(-e.PGID, 0) != nil {
					continue
				}
				if log != nil {
					log.Warn("registry", "orphan reaper: SIGKILL pgid=%d (%s)", e.PGID, e.Service)
				}
				_ = syscall.Kill(-e.PGID, syscall.SIGKILL)
			}
		}
		// Drop all our entries regardless of whether reap actually
		// fired — the previous alpha is gone, those rows are stale.
		return others, nil
	})
}

// dropByService returns entries minus any (fsHash, service) match.
func dropByService(entries []Entry, fsHash, service string) []Entry {
	out := entries[:0]
	for _, e := range entries {
		if e.FsHash == fsHash && e.Service == service {
			continue
		}
		out = append(out, e)
	}
	// Detach from input backing array.
	cp := make([]Entry, len(out))
	copy(cp, out)
	return cp
}

// withFile is the read-modify-write primitive: takes LOCK_EX on the
// adjacent .lock file, reads the registry JSON, hands it to f for
// mutation, writes the result back atomically (temp + rename so a
// crash mid-write leaves the previous good state intact).
func withFile(zordonHome string, f func([]Entry) ([]Entry, error)) error {
	regPath, release, err := openLocked(zordonHome)
	if err != nil {
		return err
	}
	defer release()

	entries, err := readEntries(regPath)
	if err != nil {
		return err
	}
	next, err := f(entries)
	if err != nil {
		return err
	}
	return writeEntries(regPath, next)
}

// withFileReadOnly avoids the rewrite cost — still takes LOCK_EX so
// the snapshot it reads is consistent with whatever a parallel writer
// is doing (no half-written file ever observed).
func withFileReadOnly(zordonHome string, f func([]Entry)) error {
	regPath, release, err := openLocked(zordonHome)
	if err != nil {
		return err
	}
	defer release()

	entries, err := readEntries(regPath)
	if err != nil {
		return err
	}
	f(entries)
	return nil
}

// openLocked makes sure <zordonHome> exists, takes LOCK_EX on
// <zordonHome>/registry.lock, and returns the registry's data path
// plus a release function. The lock is held for as long as the
// caller keeps the release closure unrun.
func openLocked(zordonHome string) (string, func(), error) {
	if zordonHome == "" {
		return "", nil, errors.New("registry: empty zordon home")
	}
	if err := os.MkdirAll(zordonHome, 0o750); err != nil {
		return "", nil, fmt.Errorf("registry: mkdir %s: %w", zordonHome, err)
	}
	lockPath := filepath.Join(zordonHome, "registry.lock")
	lf, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("registry: open lock: %w", err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		_ = lf.Close()
		return "", nil, fmt.Errorf("registry: flock: %w", err)
	}
	return filepath.Join(zordonHome, "registry.json"), func() { _ = lf.Close() }, nil
}

func readEntries(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("registry: read: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("registry: parse: %w", err)
	}
	return entries, nil
}

// writeEntries serializes via temp+rename so an os.WriteFile crash
// mid-flight can never leave a half-truncated registry. Sync to
// disk before rename for the same reason.
func writeEntries(path string, entries []Entry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: marshal: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "registry.json.tmp.*")
	if err != nil {
		return fmt.Errorf("registry: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op if rename succeeded
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("registry: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("registry: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("registry: close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("registry: rename: %w", err)
	}
	return nil
}

