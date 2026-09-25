package alphasfile

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// Tree is an entrypoint Alphasfile plus every fragment it transitively
// imports, each file loaded once. The stack is the entrypoint's modules, the
// modules its top-level imports name, and, repeated until nothing changes,
// the modules imported by any module already in the stack.
type Tree struct {
	files   []*treeFile // load order, entrypoint first
	byPath  map[string]*treeFile
	modules []*moduleBlock // instantiated, in load order
	edges   []ImportEdge
	unused  []UnusedModule

	// links holds every import per scope: DefaultModule for the entrypoint's
	// top level, a module name for the imports inside that module.
	links map[string][]importLink
	// owners maps every declared module to its file; scopes maps every scope
	// to the modules it may reference.
	owners map[string]*treeFile
	scopes map[string]map[string]bool
}

// ImportEdge is one imported file and the modules the stack takes from it,
// unioned across every importer that is part of the stack.
type ImportEdge struct {
	Path    string
	Modules []string
}

// UnusedModule is a module declared in a loaded file that is not part of
// the stack.
type UnusedModule struct {
	Path   string
	Module string
}

// LoadTree reads the entrypoint at path and follows its imports from disk.
func LoadTree(path string) (*Tree, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	t := newTree()
	if _, err := t.load(filepath.Clean(abs), nil, nil, localSource{}); err != nil {
		return nil, err
	}
	if err := t.finish(); err != nil {
		return nil, err
	}
	return t, nil
}

// ParseTree is a single inline manifest with no filesystem access. An
// import block is an error: resolving it needs a file on disk.
func ParseTree(name string, src []byte) (*Tree, error) {
	abs, _ := filepath.Abs(name)
	root, err := decodeFile(name, src)
	if err != nil {
		return nil, err
	}
	if imps := root.allImports(); len(imps) > 0 {
		return nil, fmt.Errorf("%s: import %q: imports need a file on disk; load this manifest with alphasfile.Open", imps[0].DefRange, imps[0].Path)
	}
	t := newTree()
	t.register(newTreeFile(abs, abs, src, root))
	if err := t.finish(); err != nil {
		return nil, err
	}
	return t, nil
}

// Root is the entrypoint's absolute path.
func (t *Tree) Root() string { return t.files[0].path }

// Files lists every loaded file, entrypoint first.
func (t *Tree) Files() []string {
	out := make([]string, len(t.files))
	for i, f := range t.files {
		out[i] = f.path
	}
	return out
}

// Imports lists every file the stack takes modules from, once, in load
// order. Imports inside modules that are not part of the stack are left out.
func (t *Tree) Imports() []ImportEdge { return t.edges }

// Unused lists modules of loaded files that are not part of the stack.
func (t *Tree) Unused() []UnusedModule { return t.unused }

// Bytes is the manifest identity input for invocation.ConfigHash: the
// entrypoint's bytes, then each fragment's identity and bytes. A tree
// without imports hashes exactly like the lone entrypoint did before.
func (t *Tree) Bytes() []byte {
	out := append([]byte(nil), t.files[0].src...)
	for _, f := range t.files[1:] {
		out = append(out, 0)
		out = append(out, f.identity...)
		out = append(out, 0)
		out = append(out, f.src...)
	}
	return out
}

type treeFile struct {
	path     string
	dir      string
	identity string
	src      []byte
	root     *rootBlock
	declared map[string]*moduleBlock
}

type importLink struct {
	file    *treeFile
	modules []string
}

// importSource resolves an import block to a file. Local paths are the only
// source today; remote sources plug in here.
type importSource interface {
	resolve(importer *treeFile, imp *importBlock) (path, identity string, err error)
}

type localSource struct{}

func (localSource) resolve(importer *treeFile, imp *importBlock) (string, string, error) {
	if imp.Git != nil {
		return "", "", fmt.Errorf("%s: import %q: remote imports (git {}) are not supported yet", imp.DefRange, imp.Path)
	}
	p := filepath.Clean(resolveSrcDir(importer.dir, imp.Path))
	return p, p, nil
}

func newTree() *Tree {
	return &Tree{byPath: map[string]*treeFile{}, links: map[string][]importLink{}}
}

func newTreeFile(path, identity string, src []byte, root *rootBlock) *treeFile {
	f := &treeFile{
		path:     path,
		dir:      filepath.Dir(path),
		identity: identity,
		src:      src,
		root:     root,
		declared: map[string]*moduleBlock{},
	}
	for _, mb := range root.Modules {
		f.declared[mb.Name] = mb
	}
	for _, sb := range root.allServices() {
		sb.file = f
	}
	return f
}

func (t *Tree) register(f *treeFile) {
	t.byPath[f.path] = f
	t.files = append(t.files, f)
}

// load reads one file and every file it imports. Imports of modules that
// never join the stack are loaded and checked too, so a broken fragment
// fails `zordon plan` before anyone reaches for the module.
func (t *Tree) load(path string, importer *treeFile, imp *importBlock, src importSource) (*treeFile, error) {
	if importer != nil && filepath.Base(path) == invocation.AlphasfileName {
		return nil, fmt.Errorf("%s: cannot import %q: files named %s are entrypoints and form federation levels; import a fragment such as %s.<name> instead", imp.DefRange, imp.Path, invocation.AlphasfileName, invocation.AlphasfileName)
	}
	b, err := zfs.Read(path)
	if err != nil {
		if importer == nil {
			return nil, fmt.Errorf("alphasfile read: %w", err)
		}
		return nil, fmt.Errorf("%s: import %q: %w", imp.DefRange, imp.Path, err)
	}
	root, err := decodeFile(path, b)
	if err != nil {
		return nil, err
	}
	if importer != nil {
		if err := checkFragment(root); err != nil {
			return nil, err
		}
	}
	f := newTreeFile(path, path, b, root)
	t.register(f)
	if err := t.follow(f, DefaultModule, root.Imports, src); err != nil {
		return nil, err
	}
	for _, mb := range root.Modules {
		if err := t.follow(f, mb.Name, mb.Imports, src); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (t *Tree) follow(f *treeFile, scope string, imports []*importBlock, src importSource) error {
	for _, ib := range imports {
		target, targetID, err := src.resolve(f, ib)
		if err != nil {
			return err
		}
		if len(ib.Modules) == 0 {
			return fmt.Errorf("%s: import %q: modules must name at least one module declared in %s", ib.DefRange, ib.Path, target)
		}
		tf := t.byPath[target]
		if tf == nil {
			if tf, err = t.load(target, f, ib, src); err != nil {
				return err
			}
			tf.identity = targetID
		}
		for _, m := range ib.Modules {
			if tf.declared[m] == nil {
				return fmt.Errorf("%s: import %q: module %q is not declared in %s (declared: %s)", ib.DefRange, ib.Path, m, tf.path, strings.Join(sortedKeys(tf.declared), ", "))
			}
		}
		t.links[scope] = append(t.links[scope], importLink{file: tf, modules: ib.Modules})
	}
	return nil
}

// finish checks cross-file module identity, fixes what every scope may
// reference, and computes the stack.
func (t *Tree) finish() error {
	t.owners = map[string]*treeFile{}
	for _, f := range t.files {
		for _, mb := range f.root.Modules {
			if prev, dup := t.owners[mb.Name]; dup {
				return fmt.Errorf("duplicate module %q: declared at %s and %s", mb.Name, prev.declared[mb.Name].DefRange, mb.DefRange)
			}
			t.owners[mb.Name] = f
		}
	}

	// Every module of the entrypoint is in the stack, so the entrypoint's
	// scopes see all of them. A fragment module sees itself and what it
	// imports, a sibling from its own file included.
	entry := t.files[0]
	t.scopes = map[string]map[string]bool{}
	for _, f := range t.files {
		for _, mb := range f.root.Modules {
			vis := map[string]bool{mb.Name: true}
			if f == entry {
				for name := range entry.declared {
					vis[name] = true
				}
			}
			t.scopes[mb.Name] = t.linked(vis, mb.Name)
		}
	}
	top := map[string]bool{}
	for name := range entry.declared {
		top[name] = true
	}
	t.scopes[DefaultModule] = t.linked(top, DefaultModule)

	active := map[string]bool{DefaultModule: true}
	queue := sortedKeys(t.scopes[DefaultModule])
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if active[name] {
			continue
		}
		active[name] = true
		queue = append(queue, sortedKeys(t.scopes[name])...)
	}

	for _, f := range t.files {
		if f == entry {
			t.addEdges(DefaultModule)
		}
		for _, mb := range f.root.Modules {
			if active[mb.Name] {
				t.modules = append(t.modules, mb)
				t.addEdges(mb.Name)
			} else {
				t.unused = append(t.unused, UnusedModule{Path: f.path, Module: mb.Name})
			}
		}
	}
	return nil
}

// linked adds every module the scope imports to vis.
func (t *Tree) linked(vis map[string]bool, scope string) map[string]bool {
	for _, l := range t.links[scope] {
		for _, m := range l.modules {
			vis[m] = true
		}
	}
	return vis
}

func (t *Tree) addEdges(scope string) {
	for _, l := range t.links[scope] {
		t.addEdge(l.file.path, l.modules)
	}
}

func (t *Tree) addEdge(path string, modules []string) {
	for i := range t.edges {
		if t.edges[i].Path != path {
			continue
		}
		for _, m := range modules {
			if !containsString(t.edges[i].Modules, m) {
				t.edges[i].Modules = append(t.edges[i].Modules, m)
			}
		}
		return
	}
	t.edges = append(t.edges, ImportEdge{Path: path, Modules: append([]string(nil), modules...)})
}

// visible reports whether code in scope may reference module name. A name
// no file of this tree declares, such as a federation parent's module, is
// not the tree's to hide.
func (t *Tree) visible(scope, name string) bool {
	if t.owners[name] == nil {
		return true
	}
	return t.scopes[scope][name]
}

// merged is the evaluation view: the entrypoint's root block with the
// instantiated modules of the whole tree.
func (t *Tree) merged() *rootBlock {
	rb := *t.files[0].root
	rb.Modules = t.modules
	return &rb
}

// sysenvExprs are the fragments' `sysenv` lists, unioned after the
// entrypoint's own.
func (t *Tree) sysenvExprs() []hcl.Expression {
	var out []hcl.Expression
	for _, f := range t.files[1:] {
		if f.root.SysEnvRange != (hcl.Range{}) {
			out = append(out, f.root.SysEnv)
		}
	}
	return out
}

func decodeFile(name string, src []byte) (*rootBlock, error) {
	file, diags := hclparse.NewParser().ParseHCL(src, name)
	if diags.HasErrors() {
		return nil, fmt.Errorf("alphasfile parse: %s", diags.Error())
	}
	var root rootBlock
	if diags := gohcl.DecodeBody(file.Body, nil, &root); diags.HasErrors() {
		return nil, fmt.Errorf("alphasfile decode: %s", diags.Error())
	}
	if err := annotateModules(&root); err != nil {
		return nil, err
	}
	return &root, nil
}

// checkFragment enforces what an imported file may hold: modules and
// sysenv. Everything else belongs to the entrypoint, and imports belong to
// the module that needs them.
func checkFragment(root *rootBlock) error {
	hint := fmt.Sprintf("is only allowed in the entrypoint %s; a fragment holds module blocks and sysenv", invocation.AlphasfileName)
	if len(root.Imports) > 0 {
		imp := root.Imports[0]
		return fmt.Errorf("%s: top-level import %q in a fragment; move it into the module block that uses it, so it joins the stack only with that module", imp.DefRange, imp.Path)
	}
	if len(root.Services) > 0 {
		return fmt.Errorf("%s: top-level service %s; wrap it in a module block", root.Services[0].DefRange, hint)
	}
	if root.Toolchain != nil {
		return fmt.Errorf("%s: top-level toolchain %s; pin it inside a module block", root.Toolchain.DefRange, hint)
	}
	if root.Workspace != nil {
		return fmt.Errorf("%s: top-level workspace %s", root.Workspace.DefRange, hint)
	}
	if root.EnvRange != (hcl.Range{}) {
		return fmt.Errorf("%s: top-level env %s", root.EnvRange, hint)
	}
	if root.DotenvRange != (hcl.Range{}) {
		return fmt.Errorf("%s: top-level dotenv %s", root.DotenvRange, hint)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	return slices.Contains(list, s)
}
