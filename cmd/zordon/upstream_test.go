package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

func TestRecordUpstream(t *testing.T) {
	dir := t.TempDir()
	ui := alphasfile.LockPin{Repo: "github.com/acme/ui", Ref: "HEAD", Commit: strings.Repeat("a", 40)}
	infra := alphasfile.LockPin{Repo: "github.com/acme/infra", Ref: "main", Commit: strings.Repeat("b", 40)}
	offline := alphasfile.LockPin{Repo: "github.com/acme/offline", Ref: "main", Commit: strings.Repeat("c", 40)}
	counter := fakeCounter{ui.Repo: 3, infra.Repo: 0}

	recordUpstream(dir, []alphasfile.LockPin{ui, infra, ui, offline}, counter, zlog.New(io.Discard, false))

	up, err := readUpstream(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Pins) != 2 || up.Checked.IsZero() {
		t.Fatalf("recorded %+v; want ui and infra once each, offline left out", up)
	}
	if got := up.note(ui.Repo, ui.Commit[:12]); !strings.HasPrefix(got, "behind HEAD by 3 (checked ") {
		t.Errorf("note for ui = %q", got)
	}
	if got := up.note(infra.Repo, infra.Commit); got != "" {
		t.Errorf("note for a current pin = %q, want none", got)
	}
	if got := up.note(ui.Repo, strings.Repeat("d", 40)); got != "" {
		t.Errorf("note for another commit = %q, want none: the count belongs to the pin it was taken for", got)
	}
}

func TestReadUpstream_nothingRecorded(t *testing.T) {
	up, err := readUpstream(t.TempDir())
	if err != nil || len(up.Pins) != 0 {
		t.Fatalf("got %+v, %v", up, err)
	}
}

func TestSourceNote(t *testing.T) {
	locked := strings.Repeat("a", 40)
	up := upstream{Pins: []upstreamPin{{Repo: "github.com/acme/ui", Ref: "HEAD", Commit: locked, Behind: 2}}}
	pinned := &alphasfile.Service{Package: &alphasfile.Package{Git: "https://github.com/acme/ui", Locked: locked}}
	if got := sourceNote(pinned, up); !strings.HasPrefix(got, "behind HEAD by 2") {
		t.Errorf("pinned source: %q", got)
	}
	tagged := &alphasfile.Service{Package: &alphasfile.Package{Git: "github.com/acme/ui", Tag: "v1"}}
	if got := sourceNote(tagged, up); got != "" {
		t.Errorf("tagged source: %q, want none", got)
	}
}

type fakeCounter map[string]int

func (f fakeCounter) Behind(repo, _, _ string) (int, error) {
	n, ok := f[repo]
	if !ok {
		return 0, errors.New("offline")
	}
	return n, nil
}
