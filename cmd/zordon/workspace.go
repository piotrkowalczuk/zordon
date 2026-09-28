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
	spec, err := applyWorkspaceHere(log, out, name, zordonHome)
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

// applyTarget picks the workspace `workspace apply` acts on: the --workspace
// flag when given, otherwise whichever workspace the current directory is in.
//
// Deriving from cwd is not a convenience, it is the safe default. The
// documented way to work in a workspace is to cd into it, and main's directory
// is the project root — the developer's real repository. A plain `apply` that
// assumed main would rewrite tracked files from inside a workspace that was
// never touched.
func applyTarget(flag invocation.WorkspaceName, cwd string) (invocation.WorkspaceName, error) {
	if flag != "" {
		return flag, nil
	}
	inv, err := invocation.NewInvocationState(cwd)
	if err != nil {
		return "", err
	}
	return invocation.WorkspaceName(inv.Workspace), nil
}

// runWorkspaceApply re-renders the declared files for an existing workspace.
// It is also the only way the project root ("main") ever gets them: main is
// never created, so it has no create step to hang them off.
func runWorkspaceApply(log *zlog.Logger, out io.Writer, flag invocation.WorkspaceName, zordonHome string) error {
	cwd, err := zfs.Getwd()
	if err != nil {
		return err
	}
	ws, err := applyTarget(flag, cwd)
	if err != nil {
		return err
	}
	root, base, err := workspaceBase()
	if err != nil {
		return err
	}
	if !ws.IsMain() {
		if dir := filepath.Join(base, ws.Name()); !zfs.Exists(dir) {
			return fmt.Errorf("no such workspace %q; create it with: zordon workspace create %s", ws.Name(), ws.Name())
		}
	}
	spec, err := applyWorkspaceTo(log, out, root, ws, zordonHome)
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
func applyWorkspaceHere(log *zlog.Logger, out io.Writer, name, zordonHome string) (*alphasfile.WorkspaceSpec, error) {
	root, err := projectRoot()
	if err != nil {
		return nil, err
	}
	return applyWorkspaceTo(log, out, root, invocation.WorkspaceName(name), zordonHome)
}

// renderWorkspaceHere resolves the block WITHOUT writing anything, for callers
// that only need the branch template. `workspace service add` is one: adding a
// checkout is no reason to overwrite a CLAUDE.md the user has since edited.
func renderWorkspaceHere(name string) (*alphasfile.WorkspaceSpec, error) {
	root, err := projectRoot()
	if err != nil {
		return nil, err
	}
	return renderWorkspaceFor(root, invocation.WorkspaceName(name))
}

// renderWorkspaceFor is renderWorkspaceHere with the project root supplied
// rather than discovered, so the "resolves but writes nothing" contract can be
// asserted without a cwd or a git primary.
func renderWorkspaceFor(root string, ws invocation.WorkspaceName) (*alphasfile.WorkspaceSpec, error) {
	inv, err := workspaceInvocation(root, ws)
	if err != nil {
		return nil, err
	}
	return alphasfile.RenderWorkspace(filepath.Join(root, invocation.AlphasfileName), inv)
}

// workspaceInvocation resolves the invocation for one workspace of the project
// at root, from that workspace's own directory rather than from cwd.
func workspaceInvocation(root string, ws invocation.WorkspaceName) (*invocation.InvocationState, error) {
	dir := root
	if !ws.IsMain() {
		dir = filepath.Join(root, "workspaces", ws.Name())
	}
	return invocation.NewInvocationState(dir)
}

// applyWorkspaceTo renders and writes the files for one workspace of the
// project at root.
//
// The invocation is resolved from the workspace's own directory rather than
// from cwd: `workspace create feature` runs in the project root, where cwd
// would resolve to "main". By this point the directory and its .workspace
// marker exist, so the ordinary walk-up sees exactly what it would see after
// a `cd` into it.
func applyWorkspaceTo(log *zlog.Logger, out io.Writer, root string, ws invocation.WorkspaceName, zordonHome string) (*alphasfile.WorkspaceSpec, error) {
	inv, err := workspaceInvocation(root, ws)
	if err != nil {
		return nil, err
	}
	af := filepath.Join(root, invocation.AlphasfileName)
	spec, err := alphasfile.RenderWorkspace(af, inv)
	if err != nil {
		return nil, err
	}
	protected, err := serviceSourceDirs(af, zordonHome)
	if err != nil {
		return nil, err
	}
	if err := applyWorkspaceFiles(spec, inv, protected, log, out); err != nil {
		return nil, err
	}
	return spec, nil
}

// serviceSourceDirs lists the trees the services build from, so a generated
// file can be kept out of them. In main those are the developer's own
// directories, which no path convention can identify — only the manifest can.
func serviceSourceDirs(afPath, zordonHome string) ([]string, error) {
	metas, err := parseServices(zordonHome, afPath)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range metas {
		if m.Package != nil && m.Package.Src != "" {
			out = append(out, m.Package.Src)
		}
	}
	return out, nil
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
	// Render only: `service add` needs the branch template, but rewriting the
	// workspace's files is not part of adding a checkout — run
	// `zordon workspace apply` for that.
	spec, err := renderWorkspaceHere(ws.Name())
	if err != nil {
		return err
	}
	if err := checkoutServices(ctx, log, spec, dir, picks, zordonHome); err != nil {
		return err
	}
	fmt.Fprintf(out, "added %s to workspace %q\n", strings.Join(picks, ", "), ws.Name())
	return nil
}

// checkoutTarget is one service a workspace pick checks out, at rev when
// the pick gave one.
type checkoutTarget struct{ svc, rev string }

// workspaceTargets resolves workspace picks to services. A pick names a
// service, module or package (see resolvePick); only a service may carry
// @rev. A module or package checks out its members that have a git or dir
// source, and is an error when none has.
func workspaceTargets(metas []*alphasfile.ServiceMeta, picks []string) ([]checkoutTarget, error) {
	all := metaPickables(metas)
	workspaceable := map[string]bool{}
	for _, m := range metas {
		workspaceable[m.Name] = m.Workspaceable()
	}
	var out []checkoutTarget
	seen := map[string]bool{}
	for _, raw := range picks {
		pick, rev, _ := strings.Cut(raw, "@")
		sel, err := resolvePick(all, pick)
		switch {
		case errors.Is(err, errUnknownPick):
			return nil, fmt.Errorf("unknown service, module or package %q (%s)", pick, available(all))
		case err != nil:
			return nil, err
		case !sel.single && rev != "":
			return nil, fmt.Errorf("%q: a revision names one service's checkout; pick the services of %s one by one", raw, pick)
		}
		kept := 0
		for _, name := range sel.names {
			if !sel.single && !workspaceable[name] {
				continue
			}
			kept++
			if !seen[name] {
				seen[name] = true
				out = append(out, checkoutTarget{svc: name, rev: rev})
			}
		}
		if kept == 0 {
			return nil, fmt.Errorf("%q has no service with a git or dir source; nothing to check out", pick)
		}
	}
	return out, nil
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
	metas, err := parseServices(zordonHome, af)
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
	var remove []string
	for _, pick := range svcs {
		sel, err := resolvePick(metaPickables(metas), pick)
		switch {
		case errors.Is(err, errUnknownPick):
			return fmt.Errorf("no service, module or package %q in %s", pick, af)
		case err != nil:
			return err
		case sel.single:
			if !zfs.Exists(filepath.Join(dir, "src", sel.names[0])) {
				return fmt.Errorf("service %q is not checked out in workspace %q", sel.names[0], ws.Name())
			}
			remove = append(remove, sel.names[0])
			continue
		}
		n := len(remove)
		for _, name := range sel.names {
			if zfs.Exists(filepath.Join(dir, "src", name)) {
				remove = append(remove, name)
			}
		}
		if len(remove) == n {
			return fmt.Errorf("no service of %q is checked out in workspace %q", pick, ws.Name())
		}
	}
	for _, svc := range remove {
		m := byName[svc]
		dest := filepath.Join(dir, "src", svc)
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
	metas, err := parseServices(zordonHome, af)
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
	var targets []checkoutTarget
	if len(picks) > 0 {
		if targets, err = workspaceTargets(metas, picks); err != nil {
			return err
		}
	} else {
		// No args ⇒ every workspaceable service gets a checkout.
		for _, m := range metas {
			if m.Workspaceable() {
				targets = append(targets, checkoutTarget{svc: m.Name})
			}
		}
		if len(targets) == 0 {
			log.Info("zordon", "no workspaceable services (all run from $PATH); nothing to check out")
			return nil
		}
	}
	runner := func(ctx context.Context, c *exec.Cmd) error {
		c.Stdout = os.Stderr // keep stdout clean
		c.Stderr = os.Stderr
		return c.Run()
	}
	for _, target := range targets {
		svc, rev := target.svc, target.rev
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
