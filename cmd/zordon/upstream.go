package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
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

type upstream struct {
	Pins []upstreamPin `json:"pins"`
}

// upstreamPin is how far a pin was behind its ref as of the last fetch of its
// repository, which only --pull, a missing pin and a picked checkout do.
type upstreamPin struct {
	Repo    string    `json:"repo"`
	Ref     string    `json:"ref"`
	Commit  string    `json:"commit"`
	Behind  int       `json:"behind"`
	Fetched time.Time `json:"fetched"`
}

// behindCounter counts the commits ref has that commit does not, as of the
// last fetch; source.ModFetcher answers from the bare clone without the
// network.
type behindCounter interface {
	Behind(ctx context.Context, repo, ref, commit string) (int, time.Time, error)
}

// recordUpstream counts how far each pin is behind its ref and writes the
// result to stateDir. It never fails a start: a repository not fetched yet is
// left out quietly, any other pin it cannot count is left out and logged.
func recordUpstream(ctx context.Context, stateDir string, pins []alphasfile.LockPin, counter behindCounter, log *zlog.Logger) {
	var up upstream
	seen := map[alphasfile.LockPin]bool{}
	for _, p := range pins {
		if seen[p] {
			continue
		}
		seen[p] = true
		n, fetched, err := counter.Behind(ctx, p.Repo, p.Ref, p.Commit)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			log.Warn("zordon", "%s@%s: cannot tell how far %s is behind: %v", p.Repo, p.Ref, shortSHA(p.Commit), err)
			continue
		}
		up.Pins = append(up.Pins, upstreamPin{Repo: p.Repo, Ref: p.Ref, Commit: p.Commit, Behind: n, Fetched: fetched})
	}
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

// find returns the pin of repo that follows ref, an empty ref matching any,
// and that holds commit, which may be abbreviated or empty to match any.
func (u upstream) find(repo, ref, commit string) (upstreamPin, bool) {
	for _, p := range u.Pins {
		if p.Repo == repo && (ref == "" || p.Ref == ref) && strings.HasPrefix(p.Commit, commit) {
			return p, true
		}
	}
	return upstreamPin{}, false
}

// note says how far a pin is behind its ref; empty when it is current or was
// not counted.
func (u upstream) note(repo, ref, commit string) string {
	p, ok := u.find(repo, ref, commit)
	if !ok || p.Behind == 0 {
		return ""
	}
	return fmt.Sprintf("behind %s by %d (fetched %s)", p.Ref, p.Behind, p.Fetched.Local().Format(time.DateTime))
}

// sourcePin is the pin of a service whose git source follows a branch. A
// stopped level's services come from evaluating the Alphasfile, which leaves
// Locked empty, so the pin is then found by repository and ref alone.
func (u upstream) sourcePin(s *alphasfile.Service) (upstreamPin, bool) {
	if s.Package == nil || !s.Package.FollowsBranch() {
		return upstreamPin{}, false
	}
	p, err := source.NewPrimary("", s.Package.Git, "", "", nil)
	if err != nil {
		return upstreamPin{}, false
	}
	return u.find(p.Repo, s.Package.FollowedRef(), s.Package.Locked)
}

// sourceNote says how far a service's git source is behind its ref.
func sourceNote(s *alphasfile.Service, up upstream) string {
	p, ok := up.sourcePin(s)
	if !ok {
		return ""
	}
	return up.note(p.Repo, p.Ref, p.Commit)
}
