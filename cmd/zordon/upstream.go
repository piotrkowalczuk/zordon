package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/source"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// upstreamFileName is where zordon start records, in a level's state dir, how
// far each pin of the level is behind its ref, so zordon status can say so.
const upstreamFileName = "upstream.json"

// The kinds of pin zordon.lock holds, named as its blocks are: a repository
// the Alphasfile requires, a service's git source.
const (
	pinRepo   = "repo"
	pinSource = "source"
)

type upstream struct {
	Pins []upstreamPin `json:"pins"`
}

// upstreamPin is how far a pin was behind its ref as of the last fetch of its
// repository, which only --pull, a missing pin and a picked checkout do.
type upstreamPin struct {
	Kind    string    `json:"kind"`
	Repo    string    `json:"repo"`
	Ref     string    `json:"ref"`
	Commit  string    `json:"commit"`
	Behind  int       `json:"behind,omitempty"`
	Fetched time.Time `json:"fetched"`
}

// behindCounter counts the commits ref has that commit does not, as of the
// last fetch; source.ModFetcher answers from the bare clone without the
// network.
type behindCounter interface {
	Behind(ctx context.Context, repo, ref, commit string) (int, time.Time, error)
}

// recordUpstream counts how far each pin of a level, the repositories it
// requires and its services' sources, is behind its ref and writes the result
// to stateDir. A start of some services only pins those, so what an earlier
// start recorded for the rest is kept. It never fails a start: a repository
// not fetched yet is left out quietly, any other pin it cannot count is left
// out and logged.
func recordUpstream(ctx context.Context, stateDir string, repos, sources []alphasfile.LockPin, counter behindCounter, log *zlog.Logger) {
	up, err := readUpstream(stateDir)
	if err != nil {
		log.Warn("zordon", "%v; recording anew", err)
	}
	up.count(ctx, pinRepo, repos, counter, log)
	up.count(ctx, pinSource, sources, counter, log)
	slices.SortFunc(up.Pins, func(a, b upstreamPin) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.Repo, b.Repo), strings.Compare(a.Ref, b.Ref))
	})
	b, err := json.Marshal(up)
	if err == nil {
		err = zfs.AtomicWrite(filepath.Join(stateDir, upstreamFileName), b)
	}
	if err != nil {
		log.Warn("zordon", "record how far the pins are behind: %v", err)
	}
}

// readUpstream returns what the last start of the level recorded; nothing
// when it recorded nothing.
func readUpstream(stateDir string) (upstream, error) {
	b, err := zfs.Read(filepath.Join(stateDir, upstreamFileName))
	if err != nil {
		if zfs.IsMissingErr(err) {
			return upstream{}, nil
		}
		return upstream{}, err
	}
	var up upstream
	if err := json.Unmarshal(b, &up); err != nil {
		return upstream{}, fmt.Errorf("%s: %w", upstreamFileName, err)
	}
	return up, nil
}

// count replaces what u holds for each pin of kind with its count now.
// ponytail: a pin the stack dropped lingers; nothing reads it.
func (u *upstream) count(ctx context.Context, kind string, pins []alphasfile.LockPin, counter behindCounter, log *zlog.Logger) {
	for _, p := range pins {
		pin := upstreamPin{Kind: kind, Repo: p.Repo, Ref: p.Ref, Commit: p.Commit}
		u.Pins = slices.DeleteFunc(u.Pins, pin.same)
		n, fetched, err := counter.Behind(ctx, p.Repo, p.Ref, p.Commit)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			log.Warn("zordon", "%s@%s: cannot tell how far %s is behind: %v", p.Repo, p.Ref, shortSHA(p.Commit), err)
			continue
		}
		pin.Behind, pin.Fetched = n, fetched
		u.Pins = append(u.Pins, pin)
	}
}

// behind lists the pins whose ref moved on past them.
func (u upstream) behind() []upstreamPin {
	var out []upstreamPin
	for _, p := range u.Pins {
		if p.Behind > 0 {
			out = append(out, p)
		}
	}
	return out
}

// find returns the pin of kind of repo that follows ref, an empty ref
// matching any, and that holds commit, which may be abbreviated or empty to
// match any.
func (u upstream) find(kind, repo, ref, commit string) (upstreamPin, bool) {
	for _, p := range u.Pins {
		if p.Kind == kind && p.Repo == repo && (ref == "" || p.Ref == ref) && strings.HasPrefix(p.Commit, commit) {
			return p, true
		}
	}
	return upstreamPin{}, false
}

// note says how far a pin is behind its ref; empty when it is current or was
// not counted.
func (u upstream) note(kind, repo, ref, commit string) string {
	p, ok := u.find(kind, repo, ref, commit)
	if !ok {
		return ""
	}
	return p.note()
}

// sourcePin is the pin of a service whose git source follows a branch and is
// zordon's checkout: a picked or in-place one is the developer's and has no
// pin. A stopped level's services come from evaluating the Alphasfile, which
// leaves Locked empty, so the pin is then found by repository and ref alone.
func (u upstream) sourcePin(s *alphasfile.Service) (upstreamPin, bool) {
	p := s.Package
	if p == nil || !p.FollowsBranch() || p.Editable || p.InPlace {
		return upstreamPin{}, false
	}
	prim, err := source.NewPrimary("", p.Git, "", "", nil)
	if err != nil {
		return upstreamPin{}, false
	}
	return u.find(pinSource, prim.Repo, p.FollowedRef(), p.Locked)
}

// same reports whether o is the same pin, of the same kind, repository and
// ref, whatever commit each holds.
func (p upstreamPin) same(o upstreamPin) bool {
	return p.Kind == o.Kind && p.Repo == o.Repo && p.Ref == o.Ref
}

func (p upstreamPin) note() string {
	if p.Behind == 0 {
		return ""
	}
	return fmt.Sprintf("behind %s by %d (fetched %s)", p.Ref, p.Behind, p.Fetched.Local().Format(time.DateTime))
}

// sourceNote says how far a service's git source is behind its ref.
func sourceNote(s *alphasfile.Service, up upstream) string {
	p, ok := up.sourcePin(s)
	if !ok {
		return ""
	}
	return p.note()
}
