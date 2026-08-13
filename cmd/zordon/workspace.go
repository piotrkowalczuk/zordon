package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/source"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// projectRoot returns the directory of the leaf Alphasfile (the project
// root), regardless of whether zordon was invoked from a workspace dir —
// walkUp climbs out of workspaces/<name> to <X>/Alphasfile.
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

// A workspace is just a directory <root>/workspaces/<name>/. Running
// `zordon start` from inside it gives that name its own state dir, hash,
// ports and per-service git checkouts (alpha does `git worktree add` for
// every workspaceable service at start). "main" is the implicit workspace =
// the project root itself.
//
// workspaceBase returns the project root and its workspaces directory, the
// shared setup every workspace subcommand needs.
func workspaceBase() (root, base string, err error) {
	root, err = projectRoot()
	if err != nil {
		return "", "", err
	}
	return root, filepath.Join(root, "workspaces"), nil
}

func runWorkspaceList(out io.Writer) error {
	root, base, err := workspaceBase()
	if err != nil {
		return err
	}
	entries, err := zfs.ReadDir(base)
	if err != nil {
		if zfs.IsMissingErr(err) {
			fmt.Fprintln(out, "(no workspaces; 'main' is the project root)")
			return nil
		}
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != invocation.MainWorkspace {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	fmt.Fprintf(out, "workspaces of %s:\n", root)
	fmt.Fprintln(out, "  - main (project root)")
	for _, n := range names {
		fmt.Fprintf(out, "  - %s\t%s\n", n, filepath.Join(base, n))
	}
	return nil
}

func runWorkspaceCreate(ctx context.Context, log *zlog.Logger, out io.Writer, args []string, zordonHome string) error {
	if len(args) < 1 {
		return errors.New("usage: zordon workspace create <name> [service[@rev] ...]")
	}
	name := args[0]
	if name == invocation.MainWorkspace {
		return errors.New(`"main" is the project root; just run zordon start there`)
	}
	_, base, err := workspaceBase()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, name)
	if zfs.Exists(dir) {
		return fmt.Errorf("workspace %q already exists at %s", name, dir)
	}
	if err := zfs.EnsureSharedDir(dir); err != nil {
		return err
	}
	if err := zfs.AtomicWrite(filepath.Join(dir, invocation.WorkspaceMarker), nil); err != nil {
		return err
	}
	log.Info("zordon", "created workspace %q", name)

	// Generated files land BEFORE the checkouts: an agent or a dev container
	// reads them the moment the directory exists, and while src/ is still
	// absent nothing can accidentally be written into a service's git tree.
	spec, err := applyWorkspaceHere(log, out, name)
	if err != nil {
		return err
	}

	// Materialize source checkouts. With no service args, every workspaceable
	// service gets a working tree (so the whole project is editable in this
	// workspace); pass names (optionally svc@rev) to scope it down.
	if err := checkoutServices(ctx, log, spec, dir, args[1:], zordonHome); err != nil {
		return err
	}
	fmt.Fprintf(out, "workspace ready. Bring it up with:\n  cd %s && zordon start\n", dir)
	return nil
}

// runWorkspaceApply re-renders the declared files for an existing workspace.
// It is also the only way the project root ("main") ever gets them: main is
// never created, so it has no create step to hang them off.
func runWorkspaceApply(log *zlog.Logger, out io.Writer, ws invocation.WorkspaceName) error {
	root, base, err := workspaceBase()
	if err != nil {
		return err
	}
	if !ws.IsMain() {
		if dir := filepath.Join(base, ws.Name()); !zfs.Exists(dir) {
			return fmt.Errorf("no such workspace %q; create it with: zordon workspace create %s", ws.Name(), ws.Name())
		}
	}
	spec, err := applyWorkspaceTo(log, out, root, ws)
	if err != nil {
		return err
	}
	if len(spec.Files) == 0 {
		fmt.Fprintf(out, "no workspace files declared (add a top-level workspace { file ... } block)\n")
	}
	return nil
}

// applyWorkspaceHere renders and writes the files for a named workspace of the
// project the current invocation belongs to.
func applyWorkspaceHere(log *zlog.Logger, out io.Writer, name string) (*alphasfile.WorkspaceSpec, error) {
	root, err := projectRoot()
	if err != nil {
		return nil, err
	}
	return applyWorkspaceTo(log, out, root, invocation.WorkspaceName(name))
}

// applyWorkspaceTo renders and writes the files for one workspace of the
// project at root.
//
// The invocation is resolved from the workspace's own directory rather than
// from cwd: `workspace create feature` runs in the project root, where cwd
// would resolve to "main". By this point the directory and its .workspace
// marker exist, so the ordinary walk-up sees exactly what it would see after
// a `cd` into it.
func applyWorkspaceTo(log *zlog.Logger, out io.Writer, root string, ws invocation.WorkspaceName) (*alphasfile.WorkspaceSpec, error) {
	dir := root
	if !ws.IsMain() {
		dir = filepath.Join(root, "workspaces", ws.Name())
	}
	inv, err := invocation.NewInvocationState(dir)
	if err != nil {
		return nil, err
	}
	spec, err := alphasfile.RenderWorkspace(filepath.Join(root, invocation.AlphasfileName), inv)
	if err != nil {
		return nil, err
	}
	if err := applyWorkspaceFiles(spec, inv, log, out); err != nil {
		return nil, err
	}
	return spec, nil
}

func runWorkspaceRm(log *zlog.Logger, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: zordon workspace rm <name>")
	}
	name := args[0]
	if name == invocation.MainWorkspace {
		return errors.New(`refusing to remove "main" (the project root)`)
	}
	_, base, err := workspaceBase()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, name)
	if !zfs.Exists(dir) {
		return fmt.Errorf("no such workspace %q", name)
	}
	log.Warn("zordon", "removing workspace %q (run `zordon stop` from inside first if its alpha is up)", name)
	if err := zfs.RemoveTree(dir); err != nil {
		return err
	}
	log.Info("zordon", "removed %s", dir)
	log.Info("zordon", "note: run `git worktree prune` in affected primaries to drop stale registrations")
	return nil
}

// runWorkspaceServiceAdd materializes additional service checkouts into an
// already-created workspace (`zordon workspace service add`). It reuses
// checkoutServices — the same machinery `create` uses — so an added service
// lands on the per-service branch and path `zordon start` expects.
func runWorkspaceServiceAdd(ctx context.Context, log *zlog.Logger, out io.Writer, ws invocation.WorkspaceName, picks []string, zordonHome string) error {
	if ws.IsMain() {
		return errors.New(`"main" uses the live src tree in place — no per-service checkout; pass --workspace=<name>`)
	}
	if len(picks) == 0 {
		return errors.New("usage: zordon workspace service add --workspace=<name> --services=<service[@rev] ...>")
	}
	_, base, err := workspaceBase()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, ws.Name())
	if !zfs.Exists(dir) {
		return fmt.Errorf("no such workspace %q; create it with: zordon workspace create %s", ws.Name(), ws.Name())
	}
	spec, err := applyWorkspaceHere(log, out, ws.Name())
	if err != nil {
		return err
	}
	if err := checkoutServices(ctx, log, spec, dir, picks, zordonHome); err != nil {
		return err
	}
	fmt.Fprintf(out, "added %s to workspace %q\n", strings.Join(picks, ", "), ws.Name())
	return nil
}

// runWorkspaceServiceRm detaches one or more service checkouts from an
// existing workspace (`zordon workspace service rm`), removing the git
// worktree and its tree while leaving the rest of the workspace intact.
func runWorkspaceServiceRm(ctx context.Context, log *zlog.Logger, out io.Writer, ws invocation.WorkspaceName, svcs []string, zordonHome string) error {
	if ws.IsMain() {
		return errors.New(`"main" uses the live src tree in place — no per-service checkout; pass --workspace=<name>`)
	}
	if len(svcs) == 0 {
		return errors.New("usage: zordon workspace service rm --workspace=<name> --services=<service ...>")
	}
	_, base, err := workspaceBase()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, ws.Name())
	if !zfs.Exists(dir) {
		return fmt.Errorf("no such workspace %q", ws.Name())
	}
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
	runner := func(ctx context.Context, c *exec.Cmd) error {
		c.Stdout = os.Stderr // keep stdout clean
		c.Stderr = os.Stderr
		return c.Run()
	}
	for _, svc := range svcs {
		m := byName[svc]
		if m == nil {
			return fmt.Errorf("no service %q in %s", svc, af)
		}
		dest := filepath.Join(dir, "src", svc)
		if !zfs.Exists(dest) {
			return fmt.Errorf("service %q is not checked out in workspace %q", svc, ws.Name())
		}
		p, err := source.NewPrimary(zordonHome, m.Package.Git, m.Package.Src, m.Ref(), nil)
		if err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
		log.Info("zordon", "%s: git worktree remove %s", svc, dest)
		if err := p.RemoveWorktree(ctx, dest, runner); err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
	}
	fmt.Fprintf(out, "removed %s from workspace %q\n", strings.Join(svcs, ", "), ws.Name())
	return nil
}

// checkoutServices materializes a git worktree under
// <wsdir>/src/<svc> for each picked service ("svc" or "svc@rev"). For a
// `dir` primary it `git worktree add`s from the user's repo; for a `git`
// primary it bare-clones into ~/.zordon/src on first use, then worktree-adds
// from that. The dest path and per-service branch match exactly what alpha
// resolves at `zordon start` — both come from the same
// `workspace { branch }` template — so start reuses these checkouts (and
// monorepo services sharing one primary don't collide on a shared branch).
func checkoutServices(ctx context.Context, log *zlog.Logger, spec *alphasfile.WorkspaceSpec, wsdir string, picks []string, zordonHome string) error {
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
	// Rendering every branch up front also validates the template: a
	// collision surfaces here, before the first worktree exists, rather than
	// as a git failure partway through.
	branches, err := spec.BranchesFor(metas)
	if err != nil {
		return err
	}
	explicit := len(picks) > 0
	if !explicit {
		// No args ⇒ every workspaceable service gets a checkout.
		for _, m := range metas {
			if m.Workspaceable() {
				picks = append(picks, m.Name)
			}
		}
		if len(picks) == 0 {
			log.Info("zordon", "no workspaceable services (all run from $PATH); nothing to check out")
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
		if !m.Workspaceable() {
			return fmt.Errorf("service %q has no git/dir primary; nothing to check out (it runs from $PATH)", svc)
		}
		ref := m.Ref()
		if rev != "" {
			ref = rev
		}
		var workspace *source.Workspace
		if m.Package.Workspace != nil {
			workspace = &source.Workspace{Sparse: m.Package.Workspace.Sparse}
		}
		p, err := source.NewPrimary(zordonHome, m.Package.Git, m.Package.Src, ref, workspace)
		if err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
		log.Info("zordon", "%s: ensuring primary (%s)", svc, p.Kind)
		if err := p.Ensure(ctx, runner); err != nil {
			return fmt.Errorf("%s: ensure primary: %w", svc, err)
		}
		dest := filepath.Join(wsdir, "src", svc)
		refMsg := ref
		if refMsg == "" {
			refMsg = "HEAD"
		}
		branch := branches[svc]
		log.Info("zordon", "%s: git worktree add %s @ %s (branch %s)", svc, dest, refMsg, branch)
		warn, err := p.AddWorktree(ctx, dest, branch, runner)
		if err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
		if warn != "" {
			log.Warn("zordon", "%s: %s", svc, warn)
		}
	}
	return nil
}
