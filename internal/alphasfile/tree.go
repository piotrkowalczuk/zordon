package alphasfile

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// Tree is an entrypoint Alphasfile plus every fragment and package it
// transitively imports, each file loaded once. The stack is the entrypoint's
// modules, what its imports name, and, repeated until nothing changes, what
// any module or package already in the stack imports or requires.
type Tree struct {
	files   []*treeFile // load order, entrypoint first
	byPath  map[string]*treeFile
	modules []*moduleBlock // instantiated modules and packages, in load order
	edges   []ImportEdge
	unused  []UnusedModule

	// links holds every import and require per scope: DefaultModule for the
	// entrypoint's top level, a module or package name otherwise.
	links map[string][]importLink
	// owners maps every declared module and package to its file; scopes maps
	// every scope to the modules and packages it may reference.
	owners map[string]*treeFile
	scopes map[string]map[string]bool
	active map[string]bool

	packages      map[string]*pkgInstance
	entry         *scopeSettings
	entryServices []*serviceBlock
	// disabled are the source ranges of blocks switched off by enabled; the
	// static passes over service bodies skip them.
	disabled []hcl.Range
	offs     *offIndex
	chain    map[string]bool
	src      importSource
	changes  []LockChange
}

// ImportEdge is one imported file and the modules the stack takes from it,
// unioned across every importer that is part of the stack. A package edge
// names the package and the features it was imported with.
type ImportEdge struct {
	Path     string
	Modules  []string
	Package  string
	Features []string
	// Origin is where a remote file came from: "search <dir>" or
	// "<repo>@<commit>"; empty for a local file.
	Origin string
}

// UnusedModule is a module or package of a loaded file that is not part of
// the stack.
type UnusedModule struct {
	Path   string
	Module string
}

// LoadTree reads the entrypoint at path and follows its imports from disk.
func LoadTree(path string) (*Tree, error) {
	return LoadTreeWith(path, LoadOptions{})
}

// LoadTreeWith is LoadTree with options.
func LoadTreeWith(path string, opts LoadOptions) (*Tree, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	src, err := newIdentitySource(opts)
	if err != nil {
		return nil, err
	}
	t := newTree(src)
	for _, c := range opts.Chain {
		if a, err := filepath.Abs(c); err == nil {
			t.chain[filepath.Clean(a)] = true
		}
	}
	if _, err := t.load(filepath.Clean(abs), nil, nil, nil, resolved{}); err != nil {
		return nil, err
	}
	if err := t.finish(); err != nil {
		return nil, err
	}
	if src.lock.dirty && opts.LockPath != "" {
		if err := src.lock.write(); err != nil {
			return nil, fmt.Errorf("write %s: %w", LockFileName, err)
		}
	}
	t.changes = src.changes
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
		return nil, fmt.Errorf("%s: %q: imports need a file on disk; load this manifest with alphasfile.Open", imps[0].DefRange, imps[0].Path)
	}
	t := newTree(nil)
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

// Imports lists every file the stack takes modules or a package from, once,
// in load order. Imports of scopes outside the stack are left out.
func (t *Tree) Imports() []ImportEdge { return t.edges }

// Unused lists modules and packages of loaded files that are not part of
// the stack.
func (t *Tree) Unused() []UnusedModule { return t.unused }

// LockChanges lists the lock entries this load added or moved.
func (t *Tree) LockChanges() []LockChange { return t.changes }

// Bytes is the manifest identity input for invocation.ConfigHash: the
// entrypoint's bytes, then each loaded file's identity and bytes. A tree
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
	pkg      *pkgInstance
	// confine, repoAt and origin are set for files of a remote checkout:
	// its root, "<repo>@<commit>", and a short form for display.
	confine string
	repoAt  string
	origin  string
}

// pkgInstance is a package in the stack: a directory's Alphasfile loaded
// through import or require, instantiated once as a module named after the
// alias or the directory.
type pkgInstance struct {
	name string
	file *treeFile
	at   hcl.Range
	// set is what the first import passed; every later import must pass the
	// same. A package only required keeps set nil: defaults, no features.
	set      *pkgSettings
	settings *scopeSettings
}

type pkgSettings struct {
	at       hcl.Range
	inputs   map[string]cty.Value
	features []string
}

// scopeSettings is what a package, or a runnable entrypoint, sees as
// input.<n> and feature.<n>.
type scopeSettings struct {
	inputs   map[string]cty.Value
	features map[string]bool
}

type importLink struct {
	file    *treeFile
	modules []string
	block   *importBlock
}

// isLocalPath reports whether an import path names a file on disk. Every
// other spelling is reserved for remote identities such as
// github.com/owner/repo/path.
func isLocalPath(p string) bool {
	for _, prefix := range []string{"./", "../", "/", "~/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func newTree(src importSource) *Tree {
	return &Tree{
		byPath:   map[string]*treeFile{},
		links:    map[string][]importLink{},
		packages: map[string]*pkgInstance{},
		chain:    map[string]bool{},
		src:      src,
	}
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

// load reads one file and every file it imports. Imports of scopes that
// never join the stack are loaded and checked too, so a broken file fails
// `zordon plan` before anyone reaches for it.
func (t *Tree) load(path string, importer *treeFile, imp *importBlock, pkg *pkgInstance, res resolved) (*treeFile, error) {
	if importer != nil && pkg == nil && filepath.Base(path) == invocation.AlphasfileName {
		return nil, fmt.Errorf("%s: cannot %s %q: files named %s are entrypoints and form federation levels; %s its directory to use it as a package, or a fragment such as %s.<name>", imp.DefRange, imp.keyword, imp.Path, invocation.AlphasfileName, imp.keyword, invocation.AlphasfileName)
	}
	if pkg != nil && t.chain[path] {
		return nil, fmt.Errorf("%s: cannot %s %q: %s is a federation level of this invocation, so its services would run twice", imp.DefRange, imp.keyword, imp.Path, path)
	}
	b, err := zfs.Read(path)
	if err != nil {
		if importer == nil {
			return nil, fmt.Errorf("alphasfile read: %w", err)
		}
		if pkg != nil && zfs.IsMissingErr(err) {
			return nil, fmt.Errorf("%s: %s %q: %s has no %s, so it is not a package", imp.DefRange, imp.keyword, imp.Path, filepath.Dir(path), invocation.AlphasfileName)
		}
		return nil, fmt.Errorf("%s: %s %q: %w", imp.DefRange, imp.keyword, imp.Path, err)
	}
	root, err := decodeFile(path, b)
	if err != nil {
		return nil, err
	}
	switch {
	case pkg != nil:
		err = checkPackage(root)
	case importer != nil:
		err = checkFragment(root)
	}
	if err != nil {
		return nil, err
	}
	f := newTreeFile(path, path, b, root)
	f.pkg = pkg
	f.confine, f.repoAt, f.origin = res.confine, res.repoAt, res.origin
	if pkg != nil {
		pkg.file = f
	}
	t.register(f)
	scope := DefaultModule
	if pkg != nil {
		scope = pkg.name
	}
	if err := t.follow(f, scope, "import", root.Imports); err != nil {
		return nil, err
	}
	if err := t.follow(f, scope, "require", root.Requires); err != nil {
		return nil, err
	}
	for _, mb := range root.Modules {
		if err := t.follow(f, mb.Name, "require", mb.Requires); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (t *Tree) follow(f *treeFile, scope, keyword string, blocks []*importBlock) error {
	for _, ib := range blocks {
		ib.keyword = keyword
		if err := checkImportAttrs(f, ib); err != nil {
			return err
		}
		res, err := t.src.resolve(f, ib)
		if err != nil {
			return err
		}
		if info, statErr := zfs.Stat(res.path); statErr == nil && info.IsDir() {
			if err := t.followPackage(f, scope, ib, res); err != nil {
				return err
			}
			continue
		}
		if err := t.followFragment(f, scope, ib, res); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tree) followFragment(f *treeFile, scope string, ib *importBlock, res resolved) error {
	target := res.path
	switch {
	case ib.InputsRange != (hcl.Range{}) || ib.Features != nil:
		return fmt.Errorf("%s: %s %q: inputs and features are passed to a package directory, not to a fragment file", ib.DefRange, ib.keyword, ib.Path)
	case ib.keyword == "import" && f.pkg != nil:
		return fmt.Errorf("%s: import %q: a package takes modules from a fragment with require %q { modules = [...] }", ib.DefRange, ib.Path, ib.Path)
	case ib.Modules == nil:
		return fmt.Errorf("%s: %s %q: \"modules\" is required when importing a fragment; name the modules to take from %s", ib.DefRange, ib.keyword, ib.Path, target)
	case len(ib.Modules) == 0:
		return fmt.Errorf("%s: %s %q: modules must name at least one module declared in %s", ib.DefRange, ib.keyword, ib.Path, target)
	case ib.alias != "":
		return fmt.Errorf("%s: %s %q: an alias names a package; a fragment's modules keep their declared names", ib.DefRange, ib.keyword, ib.Path)
	}
	tf := t.byPath[target]
	if tf == nil {
		var err error
		if tf, err = t.load(target, f, ib, nil, res); err != nil {
			return err
		}
		tf.identity = res.identity
	}
	if tf.pkg != nil {
		return fmt.Errorf("%s: %s %q: %s is a package's %s; %s its directory instead", ib.DefRange, ib.keyword, ib.Path, tf.path, invocation.AlphasfileName, ib.keyword)
	}
	for _, m := range ib.Modules {
		if tf.declared[m] == nil {
			return fmt.Errorf("%s: %s %q: module %q is not declared in %s (declared: %s)", ib.DefRange, ib.keyword, ib.Path, m, tf.path, strings.Join(sortedKeys(tf.declared), ", "))
		}
	}
	t.links[scope] = append(t.links[scope], importLink{file: tf, modules: ib.Modules, block: ib})
	return nil
}

func (t *Tree) followPackage(f *treeFile, scope string, ib *importBlock, res resolved) error {
	dir := res.path
	if ib.Modules != nil {
		return fmt.Errorf("%s: %s %q: a package is imported whole; drop modules", ib.DefRange, ib.keyword, ib.Path)
	}
	afPath := filepath.Join(dir, invocation.AlphasfileName)
	name := ib.alias
	if name == "" {
		name = packageName(ib.Path, dir)
	}
	if !moduleNameRe.MatchString(name) {
		return fmt.Errorf("%s: %s %q: %q is not a valid package name; give it an alias: %s %q \"<alias>\" {}", ib.DefRange, ib.keyword, ib.Path, name, ib.keyword, ib.Path)
	}
	tf := t.byPath[afPath]
	var pkg *pkgInstance
	if tf == nil {
		if prev := t.packages[name]; prev != nil {
			return fmt.Errorf("%s: %s %q: another package is already named %q (%s, imported at %s); give one of them an alias: %s %q \"<alias>\" {}", ib.DefRange, ib.keyword, ib.Path, name, prev.file.dir, prev.at, ib.keyword, ib.Path)
		}
		pkg = &pkgInstance{name: name, at: ib.DefRange}
		t.packages[name] = pkg
		var err error
		if tf, err = t.load(afPath, f, ib, pkg, res); err != nil {
			return err
		}
		tf.identity = res.identity + "/" + invocation.AlphasfileName
	} else {
		pkg = tf.pkg
		if pkg == nil {
			return fmt.Errorf("%s: %s %q: %s is the entrypoint or a fragment, not a package", ib.DefRange, ib.keyword, ib.Path, afPath)
		}
		if name != pkg.name {
			return fmt.Errorf("%s: %s %q: this package is already in the stack as %q (imported at %s); use the same name here", ib.DefRange, ib.keyword, ib.Path, pkg.name, pkg.at)
		}
	}
	if ib.keyword == "import" {
		set, err := importSettings(ib)
		if err != nil {
			return err
		}
		if pkg.set == nil {
			pkg.set = set
		} else if !sameSettings(pkg.set, set) {
			return fmt.Errorf("%s: import %q passes other inputs or features than the import at %s; a package runs once, so every import of it must pass the same", ib.DefRange, ib.Path, pkg.set.at)
		}
	}
	t.links[scope] = append(t.links[scope], importLink{file: tf, modules: []string{pkg.name}, block: ib})
	return nil
}

// finish checks cross-file identity, resolves inputs and features, fixes
// what every scope may reference, switches blocks off by enabled, and
// computes the stack.
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
	for _, f := range t.files {
		if f.pkg == nil {
			continue
		}
		if prev, dup := t.owners[f.pkg.name]; dup {
			return fmt.Errorf("%s: package %q has the same name as module %q declared at %s; import the package with an alias", f.pkg.at, f.pkg.name, f.pkg.name, prev.declared[f.pkg.name].DefRange)
		}
		t.owners[f.pkg.name] = f
	}

	entry := t.files[0]
	var err error
	if t.entry, err = resolveSettings(entry.root, nil, "this "+invocation.AlphasfileName, hcl.Range{}); err != nil {
		return err
	}
	for _, f := range t.files {
		if f.pkg != nil {
			if f.pkg.settings, err = resolveSettings(f.root, f.pkg.set, "package "+f.pkg.name, f.pkg.at); err != nil {
				return err
			}
		}
	}

	// Every module of the entrypoint is in the stack, so the entrypoint's
	// scopes see all of them. A fragment module sees itself and what it
	// requires, a sibling from its own file included; a package sees itself
	// and what it imports or requires.
	t.scopes = map[string]map[string]bool{}
	for _, f := range t.files {
		for _, mb := range f.root.Modules {
			vis := map[string]bool{mb.Name: true}
			if f == entry {
				for name := range entry.declared {
					vis[name] = true
				}
			}
			if t.scopes[mb.Name], err = t.linked(vis, mb.Name); err != nil {
				return err
			}
		}
		if f.pkg != nil {
			if t.scopes[f.pkg.name], err = t.linked(map[string]bool{f.pkg.name: true}, f.pkg.name); err != nil {
				return err
			}
		}
	}
	top := map[string]bool{}
	for name := range entry.declared {
		top[name] = true
	}
	if t.scopes[DefaultModule], err = t.linked(top, DefaultModule); err != nil {
		return err
	}

	t.active = map[string]bool{DefaultModule: true}
	queue := sortedKeys(t.scopes[DefaultModule])
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if t.active[name] {
			continue
		}
		t.active[name] = true
		queue = append(queue, sortedKeys(t.scopes[name])...)
	}

	for _, f := range t.files {
		if f == entry {
			if t.entryServices, err = t.prune(entry.root.Services, t.entry, DefaultModule); err != nil {
				return err
			}
			t.addEdges(DefaultModule)
		}
		for _, mb := range f.root.Modules {
			if !t.active[mb.Name] {
				t.unused = append(t.unused, UnusedModule{Path: f.path, Module: mb.Name})
				continue
			}
			if mb, err = t.pruneModule(mb, t.settingsFor(mb.Name)); err != nil {
				return err
			}
			t.modules = append(t.modules, mb)
			t.addEdges(mb.Name)
		}
		if f.pkg == nil {
			continue
		}
		if !t.active[f.pkg.name] {
			t.unused = append(t.unused, UnusedModule{Path: f.path, Module: f.pkg.name})
			continue
		}
		services, err := t.prune(f.root.Services, f.pkg.settings, f.pkg.name)
		if err != nil {
			return err
		}
		t.modules = append(t.modules, &moduleBlock{Name: f.pkg.name, DefRange: f.pkg.at, Toolchain: f.root.Toolchain, Services: services})
		t.addEdges(f.pkg.name)
	}
	return t.checkGated()
}

// linked adds every module and package the scope imports or requires to
// vis, skipping requires switched off by enabled.
func (t *Tree) linked(vis map[string]bool, scope string) (map[string]bool, error) {
	for _, l := range t.links[scope] {
		on, err := t.linkEnabled(scope, l)
		if err != nil {
			return nil, err
		}
		if !on {
			for _, m := range l.modules {
				if t.off().links[scope] == nil {
					t.off().links[scope] = map[string]offRecord{}
				}
				t.off().links[scope][m] = offRecord{what: "module." + m, at: l.block.EnabledRange}
			}
			continue
		}
		for _, m := range l.modules {
			vis[m] = true
		}
	}
	return vis, nil
}

func (t *Tree) linkEnabled(scope string, l importLink) (bool, error) {
	if l.block.EnabledRange == (hcl.Range{}) {
		return true, nil
	}
	return evalEnabled(l.block.Enabled, t.settingsFor(scope))
}

func (t *Tree) addEdges(scope string) {
	for _, l := range t.links[scope] {
		if on, _ := t.linkEnabled(scope, l); !on {
			continue
		}
		var features []string
		pkgName := ""
		if l.file.pkg != nil {
			pkgName = l.file.pkg.name
			if l.file.pkg.set != nil {
				features = l.file.pkg.set.features
			}
		}
		t.addEdge(l.file, l.modules, pkgName, features)
	}
}

func (t *Tree) addEdge(f *treeFile, modules []string, pkg string, features []string) {
	path := f.path
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
	t.edges = append(t.edges, ImportEdge{Path: path, Modules: append([]string(nil), modules...), Package: pkg, Features: features, Origin: f.origin})
}

// settingsFor returns the inputs and features scope sees: a package's own,
// the entrypoint's for its top level and its modules, nil elsewhere.
func (t *Tree) settingsFor(scope string) *scopeSettings {
	if pkg := t.packages[scope]; pkg != nil {
		return pkg.settings
	}
	if scope == DefaultModule || t.files[0].declared[scope] != nil {
		return t.entry
	}
	return nil
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

func (t *Tree) isDisabled(r hcl.Range) bool {
	for _, d := range t.disabled {
		if d.Filename == r.Filename && r.Start.Byte >= d.Start.Byte && r.End.Byte <= d.End.Byte {
			return true
		}
	}
	return false
}

// merged is the evaluation view: the entrypoint's root block with its
// enabled services and the instantiated modules of the whole tree.
func (t *Tree) merged() *rootBlock {
	rb := *t.files[0].root
	rb.Services = t.entryServices
	rb.Modules = t.modules
	return &rb
}

// sysenvExprs are the `sysenv` lists of the packages in the stack, unioned
// after the entrypoint's own.
func (t *Tree) sysenvExprs() []hcl.Expression {
	var out []hcl.Expression
	for _, f := range t.files[1:] {
		if f.pkg != nil && t.active[f.pkg.name] && f.root.SysEnvRange != (hcl.Range{}) {
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
	aliases := map[int]string{}
	if body, ok := file.Body.(*hclsyntax.Body); ok {
		liftAliases(body, aliases)
	}
	var root rootBlock
	if diags := gohcl.DecodeBody(file.Body, nil, &root); diags.HasErrors() {
		return nil, fmt.Errorf("alphasfile decode: %s", diags.Error())
	}
	for _, ib := range root.allImports() {
		ib.alias = aliases[ib.DefRange.Start.Byte]
	}
	if err := annotateModules(&root); err != nil {
		return nil, err
	}
	return &root, nil
}

// liftAliases moves the optional second label of import and require blocks
// into aliases, keyed by the block's start offset, so gohcl sees one label.
func liftAliases(body *hclsyntax.Body, aliases map[int]string) {
	for _, blk := range body.Blocks {
		switch blk.Type {
		case "import", "require":
			if len(blk.Labels) == 2 {
				aliases[blk.TypeRange.Start.Byte] = blk.Labels[1]
				blk.Labels = blk.Labels[:1]
				blk.LabelRanges = blk.LabelRanges[:1]
			}
		case "module":
			liftAliases(blk.Body, aliases)
		}
	}
}

// checkFragment enforces what an imported file may hold: module blocks only.
// Everything else belongs to the entrypoint or a package, and dependencies
// belong to the module that needs them.
func checkFragment(root *rootBlock) error {
	hint := fmt.Sprintf("is only allowed in the entrypoint %s or a package; a fragment holds module blocks only", invocation.AlphasfileName)
	if len(root.Imports) > 0 {
		imp := root.Imports[0]
		return fmt.Errorf("%s: import %q in a fragment; declare the dependency inside the module block that uses it as require %q { modules = [...] }, so it joins the stack only with that module", imp.DefRange, imp.Path, imp.Path)
	}
	if len(root.Requires) > 0 {
		req := root.Requires[0]
		return fmt.Errorf("%s: top-level require %q in a fragment; move it into the module block that uses it", req.DefRange, req.Path)
	}
	if len(root.Inputs) > 0 {
		return fmt.Errorf("%s: input %q %s", root.Inputs[0].DefRange, root.Inputs[0].Name, hint)
	}
	if len(root.Features) > 0 {
		return fmt.Errorf("%s: feature %q %s", root.Features[0].DefRange, root.Features[0].Name, hint)
	}
	if root.SysEnvRange != (hcl.Range{}) {
		return fmt.Errorf("%s: top-level sysenv %s", root.SysEnvRange, hint)
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

// checkPackage enforces what a package's Alphasfile may hold when it is
// imported: everything that describes the package itself, nothing that
// decides for the stack around it.
func checkPackage(root *rootBlock) error {
	hint := "is decided by the entrypoint that imports this package, not by the package"
	switch {
	case root.EnvRange != (hcl.Range{}):
		return fmt.Errorf("%s: top-level env %s; take the values as inputs", root.EnvRange, hint)
	case root.DotenvRange != (hcl.Range{}):
		return fmt.Errorf("%s: top-level dotenv %s; take the values as inputs", root.DotenvRange, hint)
	case root.Workspace != nil:
		return fmt.Errorf("%s: top-level workspace %s", root.Workspace.DefRange, hint)
	case len(root.Modules) > 0:
		return fmt.Errorf("%s: module %q in a package; a package is one module, so declare its services at the top level", root.Modules[0].DefRange, root.Modules[0].Name)
	}
	return nil
}

func checkImportAttrs(f *treeFile, ib *importBlock) error {
	if ib.keyword == "require" && (ib.InputsRange != (hcl.Range{}) || ib.Features != nil) {
		return fmt.Errorf("%s: require %q cannot pass inputs or features; import the package where the stack is composed", ib.DefRange, ib.Path)
	}
	if ib.EnabledRange == (hcl.Range{}) {
		return nil
	}
	if ib.keyword != "require" {
		return fmt.Errorf("%s: import %q: enabled is allowed on require only", ib.EnabledRange, ib.Path)
	}
	if len(f.root.Features) == 0 {
		return fmt.Errorf("%s: require %q: enabled needs a feature, and none is declared here; only a package or the entrypoint declares feature \"<name>\" {}", ib.EnabledRange, ib.Path)
	}
	return nil
}

// packageName is the local name of a package imported without an alias:
// the last segment of the import path, without a version suffix.
func packageName(label, dir string) string {
	if isLocalPath(label) {
		return filepath.Base(dir)
	}
	id, _, _ := strings.Cut(label, "@")
	return path.Base(strings.TrimSuffix(id, "/"))
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
