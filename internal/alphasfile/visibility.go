package alphasfile

import (
	"fmt"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// checkVisibility reports a `module.<m>` reference to a module of this
// manifest that the referencing file neither declares nor imports. The
// evaluation context already hides such modules; this pass exists to point
// at the expression and name the missing import.
func checkVisibility(services []*serviceBlock, owners map[string]string) error {
	for _, sb := range services {
		if sb.file == nil || sb.Body == nil {
			continue
		}
		body, ok := sb.Body.(*hclsyntax.Body)
		if !ok {
			panic(fmt.Sprintf("alphasfile: service %s body is %T, want *hclsyntax.Body", sb.Name, sb.Body))
		}
		var found error
		hclsyntax.VisitAll(body, func(n hclsyntax.Node) hcl.Diagnostics {
			if found != nil {
				return nil
			}
			st, ok := n.(*hclsyntax.ScopeTraversalExpr)
			if !ok || st.Traversal.RootName() != "module" || len(st.Traversal) < 2 {
				return nil
			}
			name, ok := traverseAttrName(st.Traversal[1])
			if !ok {
				return nil
			}
			owner, isLocal := owners[name]
			if !isLocal || sb.file.visible[name] {
				return nil
			}
			rel, err := filepath.Rel(sb.file.dir, owner)
			if err != nil {
				rel = owner
			}
			found = fmt.Errorf("%s: module.%s is not visible in %s; add import %q { modules = [%q] }", st.SrcRange, name, sb.file.path, rel, name)
			return nil
		})
		if found != nil {
			return found
		}
	}
	return nil
}
