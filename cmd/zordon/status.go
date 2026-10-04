package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/source"
)

// statusHeader is a level's first line: its Alphasfile, the invocation's
// workspace, the level's hash and whether its alpha runs.
func statusHeader(lv *level, wd string) string {
	var b strings.Builder
	b.WriteString(relPath(lv.afPath, wd))
	if lv.isInvocation {
		fmt.Fprintf(&b, "  workspace %s", lv.inv.Workspace)
	}
	fmt.Fprintf(&b, "  [%s]  ", lv.inv.FsHash)
	if st := lv.state; live(st) {
		fmt.Fprintf(&b, "alpha: running pid=%d", st.PID)
		if st.StartedAt != "" {
			fmt.Fprintf(&b, " since %s", st.StartedAt)
		}
	} else {
		b.WriteString("alpha: not running")
	}
	return b.String() + "\n"
}

// statusImports is the table of what a level imports: one row per package
// with where it comes from, who imports it and its features.
func statusImports(tree *alphasfile.Tree, wd string) string {
	if tree == nil || len(tree.Imports())+len(tree.Unused()) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("imports\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	shown := map[string]bool{}
	for _, e := range tree.Imports() {
		name := e.Package
		if name == "" {
			name = strings.Join(e.Modules, ", ")
		}
		var notes []string
		switch {
		case strings.HasPrefix(e.Origin, "search "):
			notes = append(notes, "search "+relPath(strings.TrimPrefix(e.Origin, "search "), wd))
		case e.Origin != "":
			if _, commit, ok := strings.Cut(e.Origin, "@"); ok {
				notes = append(notes, "@"+commit)
			}
		}
		if len(e.ImportedBy) > 0 {
			by := make([]string, len(e.ImportedBy))
			for i, s := range e.ImportedBy {
				_, by[i], _ = strings.Cut(s, " ")
			}
			notes = append(notes, "← "+strings.Join(by, ", "))
		}
		if len(e.Features) > 0 {
			notes = append(notes, "features: "+strings.Join(e.Features, ", "))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", name, importSource(e, wd, shown), strings.Join(notes, "   "))
	}
	for _, u := range tree.Unused() {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", u.Module, relPath(u.Path, wd), "unused: not part of the stack")
	}
	_ = tw.Flush()
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// statusServices is the table of a level's services: the name status and
// zordon start both accept, the state, and what the service prints. A level
// whose alpha does not run shows no print line, because its ports were
// picked for this report only.
func statusServices(ctx context.Context, st *protocol.StateInfo, workspace string) string {
	if len(st.Services) == 0 {
		return "services: none configured yet\n"
	}
	running := make(map[string]protocol.ServiceStatus, len(st.Running))
	for _, r := range st.Running {
		running[r.Name] = r
	}
	type row struct{ head, print, checkout string }
	rows := make([]row, len(st.Services))
	width := 0
	for i, s := range st.Services {
		state := "stopped"
		if status, ok := running[s.Name()]; ok {
			state = serviceState(ctx, s, status)
		}
		r := row{head: s.ShortName() + " — " + state, checkout: checkoutStatus(ctx, s, workspace)}
		if live(st) && s.Runtime != nil {
			r.print = s.Runtime.Print
		}
		width = max(width, len([]rune(r.head)))
		rows[i] = r
	}
	var b strings.Builder
	b.WriteString("services\n")
	for _, r := range rows {
		if r.print == "" {
			fmt.Fprintf(&b, "  %s\n", r.head)
		} else {
			fmt.Fprintf(&b, "  %s%s   %s\n", r.head, strings.Repeat(" ", width-len([]rune(r.head))), r.print)
		}
		if r.checkout != "" {
			fmt.Fprintf(&b, "      %s\n", r.checkout)
		}
	}
	return b.String()
}

// live reports whether st comes from a running alpha rather than from
// evaluating the Alphasfile for the report, which leaves the pid unset.
func live(st *protocol.StateInfo) bool { return st != nil && st.PID != 0 }

// importSource names where an import comes from: its remote identity, with
// the repository elided once an earlier row showed it, or its path relative
// to the working directory.
func importSource(e alphasfile.ImportEdge, wd string, shown map[string]bool) string {
	if e.Remote != "" {
		id := e.Remote
		if e.Package == "" {
			id += "/" + filepath.Base(e.Path)
		}
		repo, sub, _, err := source.SplitIdentity(id)
		if err != nil {
			return id
		}
		if shown[repo] && sub != "" {
			return "…/" + sub
		}
		shown[repo] = true
		return id
	}
	if e.Package != "" {
		return relPath(filepath.Dir(e.Path), wd)
	}
	return relPath(e.Path, wd)
}

// relPath spells p relative to wd when it lies under it, else as is.
func relPath(p, wd string) string {
	if wd == "" {
		return p
	}
	rel, err := filepath.Rel(wd, p)
	switch {
	case err != nil || rel == ".." || strings.HasPrefix(rel, "../"):
		return p
	case rel == ".":
		return "."
	}
	return "./" + rel
}
