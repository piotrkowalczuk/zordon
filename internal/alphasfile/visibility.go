package alphasfile

import (
	"fmt"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// checkVisibility reports a `module.<m>` reference to a module of this
// manifest that the referencing scope neither declares nor imports. The
// evaluation context already hides such modules; this pass exists to point
// at the expression and name the missing import and where it goes.
func checkVisibility(services []*serviceBlock, tree *Tree) error {
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
			owner := tree.owners[name]
			if owner == nil || tree.visible(sb.module, name) {
				return nil
			}
			rel, err := filepath.Rel(sb.file.dir, owner.path)
			if err != nil {
				rel = owner.path
			}
			scope, where := "the top level of "+sb.file.path, "at the top level"
			if sb.module != DefaultModule {
				scope, where = fmt.Sprintf("module %q (%s)", sb.module, sb.file.path), fmt.Sprintf("inside module %q", sb.module)
			}
			found = fmt.Errorf("%s: module.%s is not visible in %s; add import %q { modules = [%q] } %s", st.SrcRange, name, scope, rel, name, where)
			return nil
		})
		if found != nil {
			return found
		}
	}
	return nil
}
