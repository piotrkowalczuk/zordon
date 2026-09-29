package invocation

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// WorkFileName is the local, uncommitted file that points zordon at the
// checkouts a developer is changing.
const WorkFileName = "zordon.work"

// FindWorkFile returns the zordon.work that applies to cwd, or "" when there
// is none. It walks the physical path, symlinks resolved, up to the
// filesystem root. More than one file on the way is an error: which one wins
// is never something to guess.
func FindWorkFile(cwd string) (string, error) {
	dir, err := zfs.EvalExisting(cwd)
	if err != nil {
		return "", err
	}
	var found []string
	for {
		cand := filepath.Join(dir, WorkFileName)
		if info, err := zfs.Stat(cand); err == nil && !info.IsDir() {
			found = append(found, cand)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("found %d %s files above %s:\n  %s\nkeep exactly one", len(found), WorkFileName, cwd, strings.Join(found, "\n  "))
}
