package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

// parsePicks turns the positional args of `zordon start` into a flat,
// de-duplicated list of service names. Each arg may itself be
// comma-separated so `start a,b` and `start a b` are equivalent.
func parsePicks(args []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, a := range args {
		for _, tok := range strings.Split(a, ",") {
			name := strings.TrimSpace(tok)
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// pickServices filters `all` down to picks, transitively expanded to
// include any service referenced from a kept service's `runtime.after`
// or `provision.after`. Order in the returned slice matches the order
// in `all` so the wire shape stays deterministic. Unknown picks are an
// error — silently dropping them would lead to a confusing "no
// services configured" downstream.
//
// Transitive expansion is required because alpha's barrier resolver
// errors on `service.X.Y` refs to services not present in the
// Alphasfile it was configured with; a partial pick that drops a
// referenced dep would fail at configure time, not start it.
func pickServices(all []*alphasfile.Service, picks []string) ([]*alphasfile.Service, error) {
	byName := make(map[string]*alphasfile.Service, len(all))
	for _, s := range all {
		byName[s.Name()] = s
	}
	var unknown []string
	for _, p := range picks {
		if _, ok := byName[p]; !ok {
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown service(s): %s (available: %s)",
			strings.Join(unknown, ", "), strings.Join(sortedNames(all), ", "))
	}

	keep := map[string]struct{}{}
	queue := append([]string(nil), picks...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, done := keep[name]; done {
			continue
		}
		keep[name] = struct{}{}
		svc := byName[name]
		if svc == nil || svc.Runtime == nil {
			continue
		}
		for _, ref := range svc.Runtime.After {
			if dep, ok := serviceNameFromBarrierRef(ref); ok {
				if _, present := byName[dep]; present {
					queue = append(queue, dep)
				}
			}
		}
		for _, prov := range svc.Runtime.Provision {
			for _, ref := range prov.After {
				if dep, ok := serviceNameFromBarrierRef(ref); ok {
					if _, present := byName[dep]; present {
						queue = append(queue, dep)
					}
				}
			}
		}
	}

	out := make([]*alphasfile.Service, 0, len(keep))
	for _, s := range all {
		if _, ok := keep[s.Name()]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// serviceNameFromBarrierRef pulls the service name out of a canonical
// barrier ref. Grammar (see alpha resolveBarrier):
//
//	service.<tc>.<name>.build@<state>
//	service.<tc>.<name>.runtime@<state>
//	service.<tc>.<name>.runtime.provision.<p>@<state>
//
// Toolchain / non-service refs return ("", false).
func serviceNameFromBarrierRef(ref string) (string, bool) {
	if !strings.HasPrefix(ref, "service.") {
		return "", false
	}
	if at := strings.LastIndexByte(ref, '@'); at >= 0 {
		ref = ref[:at]
	}
	parts := strings.SplitN(strings.TrimPrefix(ref, "service."), ".", 3)
	if len(parts) < 2 || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func sortedNames(all []*alphasfile.Service) []string {
	out := make([]string, 0, len(all))
	for _, s := range all {
		out = append(out, s.Name())
	}
	sort.Strings(out)
	return out
}
