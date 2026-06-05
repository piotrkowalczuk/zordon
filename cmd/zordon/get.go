package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

// runGet resolves the federation chain (running alpha if any, else a
// static evaluation — same as `zordon status`) and prints one resolved
// value. The expression is either:
//
//	a dotted path  : service.go.prometheus.vars.address
//	a Go template  : '{{ .service.go.prometheus.vars.address }}'
//
// (anything containing "{{" is treated as a template). Scalars print
// raw; maps/slices print as compact JSON so the output is scriptable.
func runGet(ctx context.Context, args []string, zordonHome string, testCfg alphasfile.TestConfig) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return errors.New("usage: zordon get <expr>  (e.g. service.go.prometheus.vars.address)")
	}
	levels, err := resolveChain(ctx, zordonHome, testCfg)
	if err != nil {
		return err
	}
	root := buildTree(levels)
	out, err := resolveExpr(root, args[0])
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

// byName converts a list-of-entries-with-name (e.g. node["files"] =
// [{name: "config", ...}, {name: "secret", ...}]) into a map keyed by
// each entry's name, suitable for service.X.file.<name>.* access.
// Entries lacking a string `name` field are skipped. Returns an
// empty map when the source key is absent or not a list — every
// service has a `file` / `provision` namespace even if empty, so
// users can rely on the dotted path resolving (to nothing) rather
// than erroring out.
func byName(node map[string]any, srcKey string) map[string]any {
	out := map[string]any{}
	list, ok := node[srcKey].([]any)
	if !ok {
		return out
	}
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		out[name] = m
	}
	return out
}

// buildTree projects every resolved service in the chain into a generic
// tree keyed `service.<toolchain>.<name>`. Each node is the service's
// RuntimeConfig (decoded via its JSON tags: vars, arguments, env, dir,
// bin_dir, print, command, …) plus live status (pid/ready) when running.
func buildTree(levels []*level) map[string]any {
	svcRoot := map[string]any{}
	for _, lv := range levels {
		if lv.state == nil {
			continue
		}
		running := map[string]protocol.ServiceStatus{}
		for _, r := range lv.state.Running {
			running[r.Name] = r
		}
		for _, s := range lv.state.Services {
			if s == nil || s.Runtime == nil {
				continue
			}
			node := map[string]any{}
			if b, err := json.Marshal(s.Runtime); err == nil {
				_ = json.Unmarshal(b, &node)
			}
			// HCL declares `file "<name>" { ... }` and `provision "<name>"
			// { ... }` blocks — labeled with a name, mirroring the
			// service shape. The resolved Go struct stores them as a
			// list for ordering, but user-facing get paths should mirror
			// the HCL: `service.X.file.<name>.path` (keyed by the same
			// label HCL used), not `service.X.files.0.path` (positional,
			// brittle to reorder). Same for provisions.
			node["file"] = byName(node, "files")
			node["provision"] = byName(node, "provision")
			node["toolchain"] = s.Toolchain
			if s.Debugger != nil {
				if b, err := json.Marshal(s.Debugger); err == nil {
					var dbg map[string]any
					if json.Unmarshal(b, &dbg) == nil {
						node["debugger"] = dbg
					}
				}
			}
			if s.Agent != nil {
				if b, err := json.Marshal(s.Agent); err == nil {
					var ag map[string]any
					if json.Unmarshal(b, &ag) == nil {
						node["agent"] = ag
					}
				}
			}
			if st, ok := running[s.Runtime.Name]; ok {
				node["pid"] = st.PID
				node["readiness"] = st.Readiness
				node["ready"] = st.Readiness == "ready"
				node["running"] = true
			} else {
				node["running"] = false
			}
			tc, _ := svcRoot[s.Toolchain].(map[string]any)
			if tc == nil {
				tc = map[string]any{}
				svcRoot[s.Toolchain] = tc
			}
			tc[s.Runtime.Name] = node
		}
	}
	return map[string]any{"service": svcRoot}
}

// resolveExpr evaluates expr against root and returns the rendered value.
func resolveExpr(root map[string]any, expr string) (string, error) {
	expr = strings.TrimSpace(expr)
	if strings.Contains(expr, "{{") {
		t, err := template.New("get").
			Option("missingkey=error").
			Funcs(template.FuncMap{"json": toJSON}).
			Parse(expr)
		if err != nil {
			return "", fmt.Errorf("template parse: %w", err)
		}
		var sb strings.Builder
		if err := t.Execute(&sb, root); err != nil {
			return "", fmt.Errorf("template: %w", err)
		}
		return sb.String(), nil
	}
	v, err := walkPath(root, expr)
	if err != nil {
		return "", err
	}
	return scalarString(v), nil
}

// walkPath descends a dotted path. Map keys traverse objects; an integer
// segment indexes into a slice (e.g. command.0).
func walkPath(root any, path string) (any, error) {
	cur := root
	var seen []string
	for seg := range strings.SplitSeq(path, ".") {
		if seg == "" {
			return nil, fmt.Errorf("empty path segment in %q", path)
		}
		switch c := cur.(type) {
		case map[string]any:
			nv, ok := c[seg]
			if !ok {
				return nil, fmt.Errorf("no %q under %q (have: %s)",
					seg, strings.Join(seen, "."), strings.Join(sortedKeys(c), ", "))
			}
			cur = nv
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil, fmt.Errorf("index %q out of range at %q (len %d)",
					seg, strings.Join(seen, "."), len(c))
			}
			cur = c[i]
		default:
			return nil, fmt.Errorf("%q is a scalar, cannot descend into %q",
				strings.Join(seen, "."), seg)
		}
		seen = append(seen, seg)
	}
	return cur, nil
}

func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case nil:
		return ""
	default:
		return toJSON(v)
	}
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
