package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// upstreamFileName is where zordon start records, in a level's state dir, how
// far each pin of the level was behind its ref, so zordon status can say so
// without going to the network.
const upstreamFileName = "upstream.json"

type upstream struct {
	Checked time.Time     `json:"checked"`
	Pins    []upstreamPin `json:"pins"`
}

type upstreamPin struct {
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
	Behind int    `json:"behind"`
}

// behindCounter counts the commits ref has that commit does not;
// source.ModFetcher fetches to answer.
type behindCounter interface {
	Behind(repo, ref, commit string) (int, error)
}

// recordUpstream counts how far each pin is behind its ref and writes the
// result to stateDir. It never fails a start: a pin it cannot count, say
// offline, is left out and logged.
func recordUpstream(stateDir string, pins []alphasfile.LockPin, counter behindCounter, log *zlog.Logger) {
	up := upstream{Checked: time.Now().UTC()}
	seen := map[alphasfile.LockPin]bool{}
	for _, p := range pins {
		if seen[p] {
			continue
		}
		seen[p] = true
		n, err := counter.Behind(p.Repo, p.Ref, p.Commit)
		if err != nil {
			log.Warn("zordon", "%s@%s: cannot tell how far %s is behind: %v", p.Repo, p.Ref, shortSHA(p.Commit), err)
			continue
		}
		up.Pins = append(up.Pins, upstreamPin{Repo: p.Repo, Ref: p.Ref, Commit: p.Commit, Behind: n})
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

// note says how far repo at commit is behind its ref; commit may be
// abbreviated. Empty when it is current or was not counted.
func (u upstream) note(repo, commit string) string {
	if commit == "" {
		return ""
	}
	for _, p := range u.Pins {
		if p.Repo == repo && strings.HasPrefix(p.Commit, commit) && p.Behind > 0 {
			return fmt.Sprintf("behind %s by %d (checked %s)", p.Ref, p.Behind, u.Checked.Local().Format(time.DateTime))
		}
	}
	return ""
}
