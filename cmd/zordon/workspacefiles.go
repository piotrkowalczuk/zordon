package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zdoc"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// reservedStateDirs are the subtrees of a workspace's state dir that a
// generated file may never land in.
//
// src and bin belong to somebody else: src/<svc> is a git working tree, where
// a stray file would show up in that service's `git status` and risk being
// committed to its branch, and bin is build output alpha recreates.
//
// etc and var are excluded for a different reason. They are the right place
// for generated config — but the service-level file{} block already owns them,
// with the full resolved graph in scope, and alpha REMOVES the files it wrote
// there when it shuts down. A workspace file in that space would be deleted by
// a `zordon stop` it has nothing to do with.
var reservedStateDirs = []string{"src", "bin", "etc", "var"}

// reservedStateFiles are zordon's own bookkeeping inside a workspace dir.
var reservedStateFiles = []string{invocation.WorkspaceMarker, "start.lock", "alpha.log"}

// applyWorkspaceFiles materializes every declared file for one workspace.
//
// Paths are validated up front, before a single byte is written: a manifest
// with one bad path leaves the workspace untouched rather than half-written.
func applyWorkspaceFiles(spec *alphasfile.WorkspaceSpec, inv *invocation.InvocationState, log *zlog.Logger, out io.Writer) error {
	if spec == nil || len(spec.Files) == 0 {
		return nil
	}
	targets := make([]string, len(spec.Files))
	for i, f := range spec.Files {
		abs, err := workspaceFilePath(inv, f.Path)
		if err != nil {
			return fmt.Errorf("workspace.file %q: %w", f.Name, err)
		}
		targets[i] = abs
	}

	changed := 0
	for i, f := range spec.Files {
		did, err := writeWorkspaceFile(f, targets[i], log, out)
		if err != nil {
			return fmt.Errorf("workspace.file %q: %w", f.Name, err)
		}
		if did {
			changed++
		}
	}
	if changed == 0 {
		fmt.Fprintf(out, "workspace files: %d already up to date\n", len(spec.Files))
	}
	return nil
}

// writeWorkspaceFile applies one file and reports whether anything changed.
// An unchanged result is never rewritten, so a repeated apply is silent and
// leaves mtimes alone.
func writeWorkspaceFile(f *alphasfile.WorkspaceFile, abs string, log *zlog.Logger, out io.Writer) (bool, error) {
	old, existed, err := readIfPresent(abs)
	if err != nil {
		return false, err
	}

	var next []byte
	switch f.Op {
	case alphasfile.OpCreate:
		next = []byte(f.Body)
	case alphasfile.OpMerge:
		if next, err = zdoc.Merge(old, f.Data, f.Format); err != nil {
			return false, err
		}
	case alphasfile.OpRegion:
		if next, err = zdoc.Region(old, f.Name, f.Body, f.Comment); err != nil {
			return false, err
		}
	default:
		panic("zordon: unknown workspace file op " + string(f.Op))
	}

	if existed && string(old) == string(next) {
		return false, nil
	}
	if err := zfs.EnsureDir(filepath.Dir(abs)); err != nil {
		return false, err
	}
	if err := zfs.AtomicWrite(abs, next); err != nil {
		return false, err
	}
	fmt.Fprintf(out, "  %s %s\n", changeVerb(f.Op, existed), abs)
	log.Info("zordon", "workspace file %s: %s (%d bytes)", f.Name, abs, len(next))
	return true, nil
}

// changeVerb describes what happened to a file, distinguishing "zordon owns
// this whole file" from "zordon changed part of a file you own".
func changeVerb(op alphasfile.WorkspaceFileOp, existed bool) string {
	if !existed {
		return "created"
	}
	switch op {
	case alphasfile.OpMerge:
		return "merged into"
	case alphasfile.OpRegion:
		return "updated the zordon region in"
	default:
		return "rewrote"
	}
}

// workspaceFilePath resolves a manifest path against the directory the user
// actually works in, and refuses everything that belongs to zordon or to a
// service's git checkout.
func workspaceFilePath(inv *invocation.InvocationState, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative to the workspace directory", rel)
	}
	abs, ok := zfs.Resolve(inv.Dir, rel)
	if !ok {
		return "", fmt.Errorf("path %q escapes the workspace directory %s", rel, inv.Dir)
	}

	// In main the workspace dir is the project root, so the whole workspaces/
	// tree sits underneath it — and none of it is main's to write to.
	if inv.Workspace == invocation.MainWorkspace {
		if wsRoot := filepath.Join(inv.ProjectRoot(), "workspaces"); zfs.Within(wsRoot, abs) {
			return "", fmt.Errorf("path %q writes into %s, which belongs to the workspaces themselves", rel, wsRoot)
		}
	}
	for _, sub := range reservedStateDirs {
		if zfs.Within(filepath.Join(inv.StateDir, sub), abs) {
			return "", fmt.Errorf("path %q writes into %s/, which zordon and its services own; declare a file{} block inside the service instead", rel, sub)
		}
	}
	for _, name := range reservedStateFiles {
		if abs == filepath.Join(inv.StateDir, name) {
			return "", fmt.Errorf("path %q is zordon's own %s", rel, name)
		}
	}
	return abs, nil
}

func readIfPresent(path string) (body []byte, existed bool, err error) {
	b, err := zfs.Read(path)
	if err != nil {
		if zfs.IsMissingErr(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}
