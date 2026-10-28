package alphasfile

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/source"
)

// LockPin is a repository the lock holds at a commit for a ref.
type LockPin struct {
	Repo   string
	Ref    string
	Commit string
}

// PinSources pins every service of af whose git source follows a branch, or
// the default branch, to the commit zordon.lock holds for that repository and
// ref, locking the commit the ref points at now when the lock holds none. With
// pull it moves each of them to the newest commit instead. Services that build
// from one repository and ref share its pin, so they never run different
// commits of it. Picked and in-place checkouts are the developer's and are
// left alone. It returns the lock entries it added or moved, and every pin it
// set.
func PinSources(af *Alphasfile, opts LoadOptions, pull bool) ([]LockChange, []LockPin, error) {
	release, err := lockPins(opts.Home, opts.LockPath)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	lock, err := readLock(opts.LockPath)
	if err != nil {
		return nil, nil, err
	}
	var changes []LockChange
	pinned := map[sourceKey]string{}
	for _, svc := range af.All() {
		p := svc.Package
		if p == nil || !p.FollowsBranch() || p.Editable || p.InPlace {
			continue
		}
		prim, err := source.NewPrimary(opts.Home, p.Git, "", "", nil)
		if err != nil {
			return nil, nil, fmt.Errorf("service %s: %w", svc.Name(), err)
		}
		key := sourceKey{repo: prim.Repo, ref: p.FollowedRef()}
		commit, ok := pinned[key]
		if !ok {
			locked, inLock := lock.sources[key]
			commit = locked
			if !inLock || pull {
				if opts.Fetcher == nil {
					return nil, nil, fmt.Errorf("service %s: %s@%s is not in %s and fetching is off", svc.Name(), key.repo, key.ref, LockFileName)
				}
				if commit, err = opts.Fetcher.Resolve(key.repo, key.ref); err != nil {
					return nil, nil, fmt.Errorf("service %s: %w", svc.Name(), err)
				}
				if commit != locked {
					changes = append(changes, LockChange{Repo: key.repo, Ref: key.ref, Old: locked, New: commit})
					lock.sources[key] = commit
					lock.dirty = true
				}
			}
			pinned[key] = commit
		}
		p.Locked = commit
	}
	if lock.dirty && opts.LockPath != "" {
		if err := lock.write(false); err != nil {
			return nil, nil, fmt.Errorf("write %s: %w", LockFileName, err)
		}
	}
	pins := make([]LockPin, 0, len(pinned))
	for k, commit := range pinned {
		pins = append(pins, LockPin{Repo: k.repo, Ref: k.ref, Commit: commit})
	}
	return changes, sortPins(pins), nil
}

// FollowsBranch reports whether the package is a git source whose revision
// moves: one that names a branch, or none and so takes the default branch.
func (p *Package) FollowsBranch() bool {
	return p.Git != "" && p.Tag == "" && p.Rev == ""
}

// FollowedRef is the ref a source that follows a branch resolves: its
// branch, or HEAD for the default branch.
func (p *Package) FollowedRef() string {
	if p.Branch != "" {
		return p.Branch
	}
	return "HEAD"
}

// pins lists the repositories a load used, sorted.
func (l *lockFile) pins() []LockPin {
	pins := make([]LockPin, 0, len(l.used))
	for repo := range l.used {
		e := l.repos[repo]
		pins = append(pins, LockPin{Repo: repo, Ref: e.ref, Commit: e.commit})
	}
	return sortPins(pins)
}

func sortPins(pins []LockPin) []LockPin {
	slices.SortFunc(pins, func(a, b LockPin) int {
		return cmp.Or(strings.Compare(a.Repo, b.Repo), strings.Compare(a.Ref, b.Ref))
	})
	return pins
}
