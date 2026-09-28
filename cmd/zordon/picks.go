package main

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

// pickable is what a pick can name about one service.
type pickable struct {
	name   string // display name: svc, m/svc or p/m/svc
	module string // module id: "", m or p/m
	svc    string // the service's own name
}

// selection is what one pick resolved to: the display names of the
// services it selects, and whether it named a service rather than a group.
type selection struct {
	names  []string
	single bool
}

// resolvePick matches a pick against every service (its own name, display
// name and short name), module (m, or p/m of a package) and package. The
// pick must select one set of services: when two matches select different
// ones, such as two services named api or a package and a service sharing a
// name, it is an error listing each candidate.
func resolvePick(all []pickable, pick string) (selection, error) {
	type candidate struct {
		what  string
		names []string
		group bool
	}
	var found []candidate
	add := func(what string, group bool, keep func(pickable) bool) {
		var names []string
		for _, s := range all {
			if keep(s) {
				names = append(names, s.name)
			}
		}
		if len(names) > 0 {
			found = append(found, candidate{what: what, names: names, group: group})
		}
	}
	for _, s := range all {
		if pick == s.name || pick == s.svc || pick == alphasfile.ShortName(s.module, s.svc) {
			add("service "+s.name, false, func(o pickable) bool { return o.name == s.name })
		}
	}
	add("module "+pick, true, func(s pickable) bool { return s.module == pick })
	add("package "+pick, true, func(s pickable) bool {
		p, ok := alphasfile.PackageOf(s.module)
		return ok && p == pick
	})
	if len(found) == 0 {
		return selection{}, errUnknownPick
	}
	for _, c := range found[1:] {
		if !slices.Equal(c.names, found[0].names) {
			lines := make([]string, len(found))
			for i, c := range found {
				lines[i] = fmt.Sprintf("  %s: %s", c.what, strings.Join(c.names, ", "))
			}
			return selection{}, fmt.Errorf("%q names more than one thing; pick a service by its full name instead:\n%s", pick, strings.Join(lines, "\n"))
		}
	}
	sel := selection{names: found[0].names}
	for _, c := range found {
		sel.single = sel.single || !c.group
	}
	return sel, nil
}

// expandPicks resolves every pick to service display names, deduplicated,
// in the order of all. Unknown picks are one error that lists what exists.
func expandPicks(all []pickable, picks []string) ([]string, error) {
	want := map[string]bool{}
	var unknown []string
	for _, p := range picks {
		sel, err := resolvePick(all, p)
		switch {
		case errors.Is(err, errUnknownPick):
			unknown = append(unknown, p)
			continue
		case err != nil:
			return nil, err
		}
		for _, n := range sel.names {
			want[n] = true
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown service(s), module(s) or package(s): %s (%s)", strings.Join(unknown, ", "), available(all))
	}
	var out []string
	for _, s := range all {
		if want[s.name] {
			out = append(out, s.name)
		}
	}
	return out, nil
}

var errUnknownPick = errors.New("unknown pick")

func servicePickables(all []*alphasfile.Service) []pickable {
	out := make([]pickable, 0, len(all))
	for _, s := range all {
		if s.Runtime != nil {
			out = append(out, pickable{name: s.Name(), module: s.Module, svc: s.Runtime.Name})
		}
	}
	return out
}

func metaPickables(all []*alphasfile.ServiceMeta) []pickable {
	out := make([]pickable, len(all))
	for i, m := range all {
		_, svc := alphasfile.SplitDisplayName(m.Name)
		out[i] = pickable{name: m.Name, module: m.Module, svc: svc}
	}
	return out
}

// available lists what a pick may name.
func available(all []pickable) string {
	services, modules, packages := []string{}, map[string]bool{}, map[string]bool{}
	for _, s := range all {
		services = append(services, s.name)
		if s.module != "" {
			modules[s.module] = true
		}
		if p, ok := alphasfile.PackageOf(s.module); ok {
			packages[p] = true
		}
	}
	sort.Strings(services)
	out := "available services: " + strings.Join(services, ", ")
	if len(modules) > 0 {
		out += "; modules: " + strings.Join(slices.Sorted(maps.Keys(modules)), ", ")
	}
	if len(packages) > 0 {
		out += "; packages: " + strings.Join(slices.Sorted(maps.Keys(packages)), ", ")
	}
	return out
}
