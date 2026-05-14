package alphasfile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// node is a single service in the dependency graph. id is the namespace
// path used in expressions: "service.<toolchain>.<name>".
//
// Files are nested inside services (one DAG node per whole service), so
// there are no top-level file nodes. Cross-service references to a file
// resolve through the parent service: service.go.foo.file.bar.path.
type node struct {
	id      string
	service *serviceBlock
}

// graph holds nodes and their forward edges (id → set of ids the node
// depends on — i.e., must be evaluated AFTER them).
type graph struct {
	nodes []*node
	byID  map[string]*node
	deps  map[string]map[string]struct{}
}

func newGraph(services []*serviceBlock) (*graph, error) {
	g := &graph{
		byID: make(map[string]*node, len(services)),
		deps: make(map[string]map[string]struct{}, len(services)),
	}
	for _, s := range services {
		if s.Toolchain == "" || s.Name == "" {
			return nil, fmt.Errorf("service block missing toolchain or name label")
		}
		id := serviceID(s.Toolchain, s.Name)
		if _, dup := g.byID[id]; dup {
			return nil, fmt.Errorf("duplicate service %s", id)
		}
		n := &node{id: id, service: s}
		g.byID[id] = n
		g.nodes = append(g.nodes, n)
		g.deps[id] = map[string]struct{}{}
	}

	for _, n := range g.nodes {
		for _, expr := range exprsOf(n.service) {
			if expr == nil {
				continue
			}
			for _, trav := range expr.Variables() {
				dep, ok := traversalToServiceID(trav)
				if !ok {
					continue
				}
				if _, known := g.byID[dep]; !known {
					return nil, fmt.Errorf("%s references unknown service %s", n.id, dep)
				}
				if dep == n.id {
					continue // self.* is satisfied without an edge
				}
				g.deps[n.id][dep] = struct{}{}
			}
		}
	}
	return g, nil
}

// topoSort returns services in dependency-resolved order.
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

// exprsOf returns every HCL expression in a service that may carry a
// cross-service reference. Used to build the dep graph.
func exprsOf(s *serviceBlock) []hcl.Expression {
	out := []hcl.Expression{s.Vars, s.Arguments}
	for _, f := range s.Files {
		out = append(out, f.Path, f.Body)
	}
	if s.Readiness != nil && s.Readiness.HTTP != nil {
		out = append(out, s.Readiness.HTTP.Port)
	}
	return out
}

// traversalToServiceID maps a Traversal like service.go.prometheus.* to
// "service.go.prometheus". Returns false for traversals rooted elsewhere
// (self, tmpdir, etc.).
func traversalToServiceID(trav hcl.Traversal) (string, bool) {
	if len(trav) < 3 {
		return "", false
	}
	root, ok := trav[0].(hcl.TraverseRoot)
	if !ok || root.Name != "service" {
		return "", false
	}
	tc, ok1 := traverseAttrName(trav[1])
	name, ok2 := traverseAttrName(trav[2])
	if !ok1 || !ok2 {
		return "", false
	}
	return serviceID(tc, name), true
}

func traverseAttrName(s hcl.Traverser) (string, bool) {
	a, ok := s.(hcl.TraverseAttr)
	if !ok {
		return "", false
	}
	return a.Name, true
}

func serviceID(toolchain, name string) string { return "service." + toolchain + "." + name }
