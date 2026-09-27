package alphasfile

import (
	"fmt"
	"path/filepath"
	"strings"

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
			if found != nil || tree.isDisabled(n.Range()) {
				return nil
			}
			st, ok := n.(*hclsyntax.ScopeTraversalExpr)
			if !ok || len(st.Traversal) < 2 {
				return nil
			}
			name, ok := traverseAttrName(st.Traversal[1])
			if !ok {
				return nil
			}
			switch st.Traversal.RootName() {
			case "module":
				found = moduleRefError(tree, sb, st.SrcRange, name)
			case "package":
				found = packageRefError(tree, sb, st.SrcRange, name)
			}
			return nil
		})
		if found != nil {
			return found
		}
	}
	return nil
}

func moduleRefError(tree *Tree, sb *serviceBlock, at hcl.Range, name string) error {
	if p, inPkg := packageOf(sb.module); inPkg {
		if tree.owners[p+"/"+name] == nil {
			return fmt.Errorf("%s: package %q has no module %q; inside a package, module.<name> is one of its own modules, and another package's module is package.<package>.module.<name>", at, p, name)
		}
		return nil
	}
	owner := tree.owners[name]
	if owner == nil || tree.moduleVisible(sb.module, name) {
		return nil
	}
	scope, keyword, where := refScope(sb)
	return fmt.Errorf("%s: module.%s is not visible in %s; add %s %q { modules = [%q] } %s", at, name, scope, keyword, localRel(sb.file.dir, owner.path), name, where)
}

func packageRefError(tree *Tree, sb *serviceBlock, at hcl.Range, name string) error {
	pkg := tree.packages[name]
	if pkg == nil || tree.packageVisible(sb.module, name) {
		return nil
	}
	scope, keyword, where := refScope(sb)
	return fmt.Errorf("%s: package.%s is not visible in %s; add %s %q {} %s", at, name, scope, keyword, localRel(sb.file.dir, pkg.file.dir), where)
}

// refScope names where a service's reference sits and where the missing
// import or require belongs.
func refScope(sb *serviceBlock) (scope, keyword, where string) {
	if p, inPkg := packageOf(sb.module); inPkg {
		return fmt.Sprintf("package %q (%s)", p, sb.file.path), "import", fmt.Sprintf("inside package %q", p)
	}
	if sb.module != DefaultModule {
		return fmt.Sprintf("module %q (%s)", sb.module, sb.file.path), "import", fmt.Sprintf("inside module %q", sb.module)
	}
	return "the top level of " + sb.file.path, "import", "at the top level"
}

// localRel spells target relative to dir the way an import path must: with a
// leading ./ or ../, or absolute when no relative path exists.
func localRel(dir, target string) string {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return target
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return rel
	}
	return "./" + rel
}
