package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/source"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// loadOptions is how every command loads an Alphasfile: the zordon.work that
// applies to the working directory, the lock next to the Alphasfile, and a
// fetcher for remote imports.
func loadOptions(zordonHome, afPath string, chain []string) (alphasfile.LoadOptions, error) {
	cwd, err := zfs.Getwd()
	if err != nil {
		return alphasfile.LoadOptions{}, err
	}
	work, err := invocation.FindWorkFile(cwd)
	if err != nil {
		return alphasfile.LoadOptions{}, err
	}
	var search []string
	if work != "" {
		if search, err = alphasfile.ReadWorkFile(work); err != nil {
			return alphasfile.LoadOptions{}, err
		}
	}
	return alphasfile.LoadOptions{
		Chain:    chain,
		Home:     zordonHome,
		Search:   search,
		LockPath: filepath.Join(filepath.Dir(afPath), alphasfile.LockFileName),
		Fetcher:  source.ModFetcher{Home: zordonHome},
	}, nil
}

// loadTree loads afPath the way every command does.
func loadTree(zordonHome, afPath string, chain []string) (*alphasfile.Tree, error) {
	opts, err := loadOptions(zordonHome, afPath, chain)
	if err != nil {
		return nil, err
	}
	return alphasfile.LoadTreeWith(afPath, opts)
}

// parseServices is alphasfile.ParseServices with the command's load options.
func parseServices(zordonHome, afPath string) ([]*alphasfile.ServiceMeta, error) {
	opts, err := loadOptions(zordonHome, afPath, nil)
	if err != nil {
		return nil, err
	}
	return alphasfile.ParseServicesWith(afPath, opts)
}

// runUpdate re-resolves the remote repositories the invocation's Alphasfile
// imports, all of them or the ones named, and rewrites zordon.lock.
func runUpdate(out io.Writer, zordonHome string, repos []string) error {
	af, err := walkUp()
	if err != nil {
		return err
	}
	opts, err := loadOptions(zordonHome, af, nil)
	if err != nil {
		return err
	}
	return updateLock(out, af, opts, repos)
}

func updateLock(out io.Writer, af string, opts alphasfile.LoadOptions, repos []string) error {
	opts.Update = repos
	opts.UpdateAll = len(repos) == 0
	tree, err := alphasfile.LoadTreeWith(af, opts)
	if err != nil {
		return err
	}
	changes := tree.LockChanges()
	if len(changes) == 0 {
		_, err := fmt.Fprintf(out, "%s is up to date\n", alphasfile.LockFileName)
		return err
	}
	for _, c := range changes {
		old := c.Old
		if old == "" {
			old = "(new)"
		}
		if _, err := fmt.Fprintf(out, "%s@%s: %s -> %s\n", c.Repo, c.Ref, shortSHA(old), shortSHA(c.New)); err != nil {
			return err
		}
	}
	return nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
