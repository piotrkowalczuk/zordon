package alphasfile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// nodeKind identifies which producer field of a service a graph node
// represents. Producers are exactly the four fields whose evaluated
// values get exposed back to HCL via self.<kind>... and
// service.<tc>.<svc>.<kind>... traversals: vars, arguments, env, and
// per-file objects. Sinks (runtime.cmd, build.cmd, sudo, provisions,
// readiness, …) are NOT producers — nothing reads them back, so they
// don't need DAG nodes; they run in a single post-producer pass.
type nodeKind int

const (
	kindVars nodeKind = iota
	kindArguments
	kindEnv
	kindFile
)

func (k nodeKind) String() string {
	switch k {
	case kindVars:
		return "vars"
	case kindArguments:
		return "arguments"
	case kindEnv:
		return "env"
	case kindFile:
		return "file"
	}
	return "?"
}

// node is one producer of one service. Per-producer (not per-service)
// granularity is what lets two services cross-reference each other's
// vars and env without a false cycle: A.env→B.vars and B.env→A.vars
// are independent nodes, so the only edges are A.env→B.vars and
// B.env→A.vars (not A→B and B→A as whole services).
type node struct {
	id    string // "service.<tc>.<svc>.vars" | ".arguments" | ".env" | ".file.<name>"
	svc   *serviceBlock
	svcID string // "service.<tc>.<svc>"
	kind  nodeKind
	name  string // file name (kindFile only)
	exprs []hcl.Expression
}

// graph holds nodes and forward edges (id → set of ids it depends on —
// i.e., must be evaluated AFTER them).
type graph struct {
	nodes []*node
	byID  map[string]*node
	deps  map[string]map[string]struct{}
}

func nodeID(svcID string, kind nodeKind, name string) string {
	if kind == kindFile {
		return svcID + ".file." + name
	}
	return svcID + "." + kind.String()
}

// newGraph builds the cross-service per-producer DAG. parentKnown lists
// services already resolved by a federation parent; references to those
// are valid but never carry an intra-graph edge (the parent was
// evaluated end-to-end before this graph runs).
func newGraph(services []*serviceBlock, parentKnown map[string]struct{}) (*graph, error) {
	g := &graph{
		byID: map[string]*node{},
		deps: map[string]map[string]struct{}{},
	}
	seen := map[string]struct{}{}
	for _, s := range services {
		if s.Toolchain == "" || s.Name == "" {
			return nil, fmt.Errorf("service block missing toolchain or name label")
		}
		sid := serviceID(s.Toolchain, s.Name)
		if _, dup := seen[sid]; dup {
			return nil, fmt.Errorf("duplicate service %s", sid)
		}
		seen[sid] = struct{}{}
		add := func(kind nodeKind, name string, exprs ...hcl.Expression) {
			any := false
			for _, e := range exprs {
				if e != nil {
					any = true
					break
				}
			}
			if !any && kind != kindVars && kind != kindArguments && kind != kindEnv {
				// Files only exist if declared; producer placeholders for
				// vars/arguments/env are always added so other nodes can
				// resolve self.<kind> to the empty value uniformly.
				return
			}
			id := nodeID(sid, kind, name)
			n := &node{id: id, svc: s, svcID: sid, kind: kind, name: name, exprs: exprs}
			g.byID[id] = n
			g.nodes = append(g.nodes, n)
			g.deps[id] = map[string]struct{}{}
		}
		add(kindVars, "", s.Vars)
		var argVals hcl.Expression
		if s.Arguments != nil {
			argVals = s.Arguments.Values
		}
		add(kindArguments, "", argVals)
		add(kindEnv, "", s.Env)
		for _, fb := range s.Files {
			add(kindFile, fb.Name, fb.Path, fb.Body)
		}
	}

	for _, n := range g.nodes {
		for _, expr := range n.exprs {
			if expr == nil {
				continue
			}
			for _, trav := range expr.Variables() {
				_, depID, ok := producerNodeFromTrav(trav, n.svcID)
				if !ok {
					continue
				}
				if _, local := g.byID[depID]; !local {
					// Either a parent-resolved service (no intra-graph edge
					// needed) or a missing producer (eval-time HCL diagnostic
					// will report it precisely). Either way, skip.
					continue
				}
				if depID == n.id {
					continue // self-ref to same producer node — handled at eval time
				}
				g.deps[n.id][depID] = struct{}{}
			}
		}
	}
	return g, nil
}

// producerNodeFromTrav maps a `self.<f>...` or `service.<tc>.<svc>.<f>...`
// traversal to the producer node id it depends on. Returns the target
// service id and node id. Static traversals (barrier states under
// runtime/build, scalar self.{name,toolchain,dir}, parent toolchain
// refs) and non-service roots (fs::, cfg::, the `never` keyword, …)
// produce no edge.
func producerNodeFromTrav(t hcl.Traversal, selfSvcID string) (svc, id string, ok bool) {
	if len(t) < 2 {
		return "", "", false
	}
	root, isRoot := t[0].(hcl.TraverseRoot)
	if !isRoot {
		return "", "", false
	}
	var rest hcl.Traversal
	switch root.Name {
	case "self":
		svc = selfSvcID
		rest = t[1:]
	case "service":
		if len(t) < 4 {
			return "", "", false
		}
		tc, ok1 := traverseAttrName(t[1])
		nm, ok2 := traverseAttrName(t[2])
		if !ok1 || !ok2 {
			return "", "", false
		}
		svc = serviceID(tc, nm)
		rest = t[3:]
	default:
		return "", "", false
	}
	if len(rest) == 0 {
		return "", "", false
	}
	field, fok := traverseAttrName(rest[0])
	if !fok {
		return "", "", false
	}
	switch field {
	case "vars":
		return svc, nodeID(svc, kindVars, ""), true
	case "arguments":
		return svc, nodeID(svc, kindArguments, ""), true
	case "env":
		return svc, nodeID(svc, kindEnv, ""), true
	case "file":
		if len(rest) < 2 {
			return "", "", false
		}
		fname, ok := traverseAttrName(rest[1])
		if !ok {
			return "", "", false
		}
		return svc, nodeID(svc, kindFile, fname), true
	}
	// name / toolchain / dir / runtime.<state> / build.<state> /
	// runtime.provision.<n>.<state>: all static, no producer edge.
	return "", "", false
}

// topoSort returns nodes in dependency-resolved order. Lexical id
// breaks ties so the order is stable across runs (deterministic
// fs::pickport allocation in tests, predictable error messages).
func (g *graph) topoSort() ([]*node, error) {
	indeg := make(map[string]int, len(g.nodes))
	rev := make(map[string][]string, len(g.nodes))
	for id, deps := range g.deps {
		for d := range deps {
			rev[d] = append(rev[d], id)
			indeg[id]++
		}
		if _, ok := indeg[id]; !ok {
			indeg[id] = 0
		}
	}

	ready := make([]string, 0)
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	out := make([]*node, 0, len(g.nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, g.byID[id])
		nxt := rev[id]
		sort.Strings(nxt)
		for _, next := range nxt {
			indeg[next]--
			if indeg[next] == 0 {
				ready = append(ready, next)
				sort.Strings(ready)
			}
		}
	}
	if len(out) != len(g.nodes) {
		return nil, fmt.Errorf("cycle in Alphasfile dependency graph (involves %s)", cycleMembers(indeg))
	}
	return out, nil
}

func cycleMembers(indeg map[string]int) string {
	var stuck []string
	for id, d := range indeg {
		if d > 0 {
			stuck = append(stuck, id)
		}
	}
	sort.Strings(stuck)
	return strings.Join(stuck, ", ")
}

func traverseAttrName(s hcl.Traverser) (string, bool) {
	a, ok := s.(hcl.TraverseAttr)
	if !ok {
		return "", false
	}
	return a.Name, true
}

func serviceID(toolchain, name string) string { return "service." + toolchain + "." + name }
