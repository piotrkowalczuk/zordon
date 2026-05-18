package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/source"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// projectRoot returns the directory of the leaf Alphasfile (the project
// root), regardless of whether zordon was invoked from a worktree dir —
// walkUp climbs out of .zordon/worktrees/<name> to <X>/Alphasfile.
func projectRoot() (string, error) {
	af, err := walkUp()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(af)
	if err != nil {
		return "", err
	}
	return filepath.Dir(abs), nil
}

// runWorktree implements `zordon worktree <create|list|rm> [name]`.
//
// A worktree is just a directory <root>/.zordon/worktrees/<name>/. Running
// `zordon start` from inside it gives that name its own state dir, hash,
// ports and per-service git checkouts (alpha does `git worktree add` for
// every worktree-able service at start). "main" is the implicit worktree =
// the project root itself.
func runWorktree(ctx context.Context, log *zlog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: zordon worktree <create|list|rm> [name]")
	}
	root, err := projectRoot()
	if err != nil {
		return err
	}
	base := filepath.Join(root, ".zordon", "worktrees")

	switch args[0] {
	case "list":
		entries, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("(no worktrees; 'main' is the project root)")
				return nil
			}
			return err
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() && e.Name() != invocation.MainWorktree {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		fmt.Printf("worktrees of %s:\n", root)
		fmt.Println("  - main (project root)")
		for _, n := range names {
			fmt.Printf("  - %s\t%s\n", n, filepath.Join(base, n))
		}
		return nil

	case "create":
		if len(args) < 2 {
			return errors.New("usage: zordon worktree create <name> [service[@rev] ...]")
		}
		name := args[1]
		if name == invocation.MainWorktree {
			return errors.New(`"main" is the project root; just run zordon start there`)
		}
		dir := filepath.Join(base, name)
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("worktree %q already exists at %s", name, dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		log.Info("zordon", "created worktree %q", name)

		// Materialize source checkouts. With no service args, every
		// worktree-able service gets a working tree (so the whole project
		// is editable in this worktree); pass names (optionally svc@rev)
		// to scope it down.
		if err := checkoutServices(ctx, log, name, dir, args[2:]); err != nil {
			return err
		}
		fmt.Printf("worktree ready. Bring it up with:\n  cd %s && zordon start\n", dir)
		return nil

	case "rm":
		if len(args) < 2 {
			return errors.New("usage: zordon worktree rm <name>")
		}
		name := args[1]
		if name == invocation.MainWorktree {
			return errors.New(`refusing to remove "main" (the project root)`)
		}
		dir := filepath.Join(base, name)
		if _, err := os.Stat(dir); err != nil {
			return fmt.Errorf("no such worktree %q", name)
		}
		log.Warn("zordon", "removing worktree %q (run `zordon stop` from inside first if its alpha is up)", name)
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		log.Info("zordon", "removed %s", dir)
		log.Info("zordon", "note: run `git worktree prune` in affected primaries to drop stale registrations")
		return nil

	default:
		return fmt.Errorf("unknown worktree subcommand %q (create|list|rm)", args[0])
	}
}

// checkoutServices materializes a git worktree under
// <wtdir>/src/<svc> for each picked service ("svc" or "svc@rev"). For a
// `dir` primary it `git worktree add`s from the user's repo; for a `git`
// primary it bare-clones into ~/.zordon/src on first use, then worktree-adds
// from that. The dest path and per-service branch (zordon/<name>/<svc>)
// match exactly what alpha computes at `zordon start`, so start reuses
// these checkouts (and monorepo services sharing one primary don't
// collide on a shared branch).
func checkoutServices(ctx context.Context, log *zlog.Logger, name, wtdir string, picks []string) error {
	af, err := walkUp()
	if err != nil {
		return err
	}
	metas, err := alphasfile.ParseServices(af)
	if err != nil {
		return err
	}
	byName := make(map[string]*alphasfile.ServiceMeta, len(metas))
	for _, m := range metas {
		byName[m.Name] = m
	}
	explicit := len(picks) > 0
	if !explicit {
		// No args ⇒ every worktree-able service gets a checkout.
		for _, m := range metas {
			if m.Worktreeable() {
				picks = append(picks, m.Name)
			}
		}
		if len(picks) == 0 {
			log.Info("zordon", "no worktree-able services (all run from $PATH); nothing to check out")
			return nil
		}
	}
	runner := func(ctx context.Context, c *exec.Cmd) error {
		c.Stdout = os.Stderr // keep stdout clean
		c.Stderr = os.Stderr
		return c.Run()
	}
	for _, pick := range picks {
		svc, rev, _ := strings.Cut(pick, "@")
		m := byName[svc]
		if m == nil {
			return fmt.Errorf("no service %q in %s", svc, af)
		}
		if !m.Worktreeable() {
			return fmt.Errorf("service %q has no git/dir primary; nothing to check out (it runs from $PATH)", svc)
		}
		ref := m.Ref()
		if rev != "" {
			ref = rev
		}
		var worktree *source.Worktree
		if m.Package.Worktree != nil {
			worktree = &source.Worktree{Sparse: m.Package.Worktree.Sparse}
		}
		p, err := source.NewPrimary(m.Package.Git, m.Package.Src, ref, worktree)
		if err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
		log.Info("zordon", "%s: ensuring primary (%s)", svc, p.Kind)
		if err := p.Ensure(ctx, runner); err != nil {
			return fmt.Errorf("%s: ensure primary: %w", svc, err)
		}
		dest := filepath.Join(wtdir, "src", svc)
		refMsg := ref
		if refMsg == "" {
			refMsg = "HEAD"
		}
		branch := "zordon/" + name + "/" + svc
		log.Info("zordon", "%s: git worktree add %s @ %s (branch %s)", svc, dest, refMsg, branch)
		if err := p.AddWorktree(ctx, dest, branch, runner); err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
	}
	return nil
}
