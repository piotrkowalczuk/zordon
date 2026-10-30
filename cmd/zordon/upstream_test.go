package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

func TestRecordUpstream(t *testing.T) {
	dir := t.TempDir()
	ui := alphasfile.LockPin{Repo: "github.com/acme/ui", Ref: "HEAD", Commit: strings.Repeat("a", 40)}
	infra := alphasfile.LockPin{Repo: "github.com/acme/infra", Ref: "main", Commit: strings.Repeat("b", 40)}
	never := alphasfile.LockPin{Repo: "github.com/acme/never", Ref: "main", Commit: strings.Repeat("c", 40)}
	broken := alphasfile.LockPin{Repo: "github.com/acme/broken", Ref: "main", Commit: strings.Repeat("d", 40)}
	counter := fakeCounter{ui.Repo: 3, infra.Repo: 0}

	recordUpstream(t.Context(), dir, []alphasfile.LockPin{infra, never}, []alphasfile.LockPin{ui, broken}, counter, zlog.New(io.Discard, false))

	up, err := readUpstream(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Pins) != 2 {
		t.Fatalf("recorded %+v; want ui and infra, the unfetched and the broken one left out", up)
	}
	if got := up.note(pinSource, ui.Repo, "", ui.Commit[:12]); !strings.HasPrefix(got, "behind HEAD by 3 (fetched ") {
		t.Errorf("note for ui = %q", got)
	}
	if got := up.note(pinRepo, infra.Repo, "main", infra.Commit); got != "" {
		t.Errorf("note for a current pin = %q, want none", got)
	}
	if got := up.note(pinRepo, ui.Repo, "", ui.Commit); got != "" {
		t.Errorf("note for ui as a repository pin = %q, want none: it is a source pin", got)
	}
	if got := up.note(pinSource, ui.Repo, "", strings.Repeat("e", 40)); got != "" {
		t.Errorf("note for another commit = %q, want none: the count belongs to the pin it was taken for", got)
	}
	if got := up.behind(); len(got) != 1 || got[0].Repo != ui.Repo {
		t.Errorf("behind() = %+v, want ui alone", got)
	}
}

func TestRecordUpstream_keepsThePinsOfServicesNotStarted(t *testing.T) {
	dir := t.TempDir()
	ui := alphasfile.LockPin{Repo: "github.com/acme/ui", Ref: "HEAD", Commit: strings.Repeat("a", 40)}
	web := alphasfile.LockPin{Repo: "github.com/acme/web", Ref: "HEAD", Commit: strings.Repeat("b", 40)}
	log := zlog.New(io.Discard, false)

	recordUpstream(t.Context(), dir, nil, []alphasfile.LockPin{ui, web}, fakeCounter{ui.Repo: 1, web.Repo: 1}, log)
	recordUpstream(t.Context(), dir, nil, []alphasfile.LockPin{ui}, fakeCounter{ui.Repo: 0}, log)

	up, err := readUpstream(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := up.note(pinSource, ui.Repo, "HEAD", ui.Commit); got != "" {
		t.Errorf("note for ui = %q, want none: the start counted it again", got)
	}
	if got := up.note(pinSource, web.Repo, "HEAD", web.Commit); !strings.HasPrefix(got, "behind HEAD by 1") {
		t.Errorf("note for web = %q, want the count of the start that had it", got)
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
	up := upstream{Pins: []upstreamPin{
		{Kind: pinSource, Repo: "github.com/acme/ui", Ref: "HEAD", Commit: locked, Behind: 2},
		{Kind: pinSource, Repo: "github.com/acme/ui", Ref: "release", Commit: locked, Behind: 0},
		{Kind: pinRepo, Repo: "github.com/acme/ui", Ref: "main", Commit: locked, Behind: 5},
	}}
	cases := map[string]struct {
		pkg  alphasfile.Package
		want string
	}{
		"default-branch":  {alphasfile.Package{Git: "https://github.com/acme/ui", Locked: locked}, "behind HEAD by 2"},
		"other-ref":       {alphasfile.Package{Git: "github.com/acme/ui", Branch: "release", Locked: locked}, ""},
		"stopped-level":   {alphasfile.Package{Git: "github.com/acme/ui"}, "behind HEAD by 2"},
		"tagged":          {alphasfile.Package{Git: "github.com/acme/ui", Tag: "v1"}, ""},
		"picked":          {alphasfile.Package{Git: "github.com/acme/ui", Editable: true}, ""},
		"in-place":        {alphasfile.Package{Git: "github.com/acme/ui", InPlace: true}, ""},
		"required-is-not": {alphasfile.Package{Git: "github.com/acme/ui", Branch: "main"}, ""},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got := sourceNote(&alphasfile.Service{Package: &c.pkg}, up)
			if !strings.HasPrefix(got, c.want) || (c.want == "") != (got == "") {
				t.Errorf("got %q, want prefix %q", got, c.want)
			}
		})
	}
}

type fakeCounter map[string]int

func (f fakeCounter) Behind(_ context.Context, repo, _, _ string) (int, time.Time, error) {
	if repo == "github.com/acme/never" {
		return 0, time.Time{}, fmt.Errorf("not fetched yet: %w", fs.ErrNotExist)
	}
	n, ok := f[repo]
	if !ok {
		return 0, time.Time{}, errors.New("broken")
	}
	return n, time.Now(), nil
}
