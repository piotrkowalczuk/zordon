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
// imports, each file loaded once. Only modules that the entrypoint declares
// or that some loaded file names in an import are instantiated.
type Tree struct {
	files   []*treeFile // load order, entrypoint first
	byPath  map[string]*treeFile
	modules []*moduleBlock // instantiated, in load order
	edges   []ImportEdge
	unused  []UnusedModule
}

// ImportEdge is one imported file and the modules the manifest takes from
// it, unioned across every importer.
type ImportEdge struct {
	Path    string
	Modules []string
}

// UnusedModule is a module declared in a loaded fragment that no import
// names, so it is not part of the manifest.
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
	t := &Tree{byPath: map[string]*treeFile{}}
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
	if len(root.Imports) > 0 {
		imp := root.Imports[0]
		return nil, fmt.Errorf("%s: import %q: imports need a file on disk; load this manifest with alphasfile.Open", imp.DefRange, imp.Path)
	}
	t := &Tree{byPath: map[string]*treeFile{}}
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

// Imports lists every imported file once, in load order, with the modules
// taken from it.
func (t *Tree) Imports() []ImportEdge { return t.edges }

// Unused lists modules of loaded fragments that nothing imports.
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
	// visible holds the modules this file may reference: the ones it
	// declares plus the ones it imports.
	visible map[string]bool
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

func newTreeFile(path, identity string, src []byte, root *rootBlock) *treeFile {
	f := &treeFile{
		path:     path,
		dir:      filepath.Dir(path),
		identity: identity,
		src:      src,
		root:     root,
		declared: map[string]*moduleBlock{},
		visible:  map[string]bool{},
	}
	for _, mb := range root.Modules {
		f.declared[mb.Name] = mb
		f.visible[mb.Name] = true
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
	for _, ib := range root.Imports {
		target, targetID, err := src.resolve(f, ib)
		if err != nil {
			return nil, err
		}
		if len(ib.Modules) == 0 {
			return nil, fmt.Errorf("%s: import %q: modules must name at least one module declared in %s", ib.DefRange, ib.Path, target)
		}
		tf := t.byPath[target]
		if tf == nil {
			if tf, err = t.load(target, f, ib, src); err != nil {
				return nil, err
			}
			tf.identity = targetID
		}
		for _, m := range ib.Modules {
			if tf.declared[m] == nil {
				return nil, fmt.Errorf("%s: import %q: module %q is not declared in %s (declared: %s)", ib.DefRange, ib.Path, m, tf.path, strings.Join(sortedKeys(tf.declared), ", "))
			}
			f.visible[m] = true
		}
		t.addEdge(tf.path, ib.Modules)
	}
	return f, nil
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

// finish checks cross-file module identity and fixes the instantiated set.
func (t *Tree) finish() error {
	owner := map[string]*moduleBlock{}
	for _, f := range t.files {
		for _, mb := range f.root.Modules {
			if prev, dup := owner[mb.Name]; dup {
				return fmt.Errorf("duplicate module %q: declared at %s and %s", mb.Name, prev.DefRange, mb.DefRange)
			}
			owner[mb.Name] = mb
		}
	}
	named := map[string]bool{}
	for name := range t.files[0].declared {
		named[name] = true
	}
	for _, e := range t.edges {
		for _, m := range e.Modules {
			named[m] = true
		}
	}
	for _, f := range t.files {
		for _, mb := range f.root.Modules {
			if named[mb.Name] {
				t.modules = append(t.modules, mb)
			} else {
				t.unused = append(t.unused, UnusedModule{Path: f.path, Module: mb.Name})
			}
		}
	}
	return nil
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

// moduleOwners maps every instantiated module to the file declaring it.
func (t *Tree) moduleOwners() map[string]string {
	out := map[string]string{}
	for _, mb := range t.modules {
		for _, f := range t.files {
			if f.declared[mb.Name] == mb {
				out[mb.Name] = f.path
			}
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

// checkFragment enforces what an imported file may hold: import, module and
// sysenv. Everything else belongs to the entrypoint.
func checkFragment(root *rootBlock) error {
	hint := fmt.Sprintf("is only allowed in the entrypoint %s; a fragment holds import, module and sysenv", invocation.AlphasfileName)
	if len(root.Services) > 0 {
		return fmt.Errorf("%s: top-level service %s; wrap it in a module block", root.Services[0].DefRange, hint)
	}
	if root.Toolchain != nil {
		return fmt.Errorf("%s: top-level toolchain %s; pin it inside a module block", root.Toolchain.DefRange, hint)
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
