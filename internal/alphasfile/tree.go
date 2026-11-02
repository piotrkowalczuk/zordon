package alphasfile

import (
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// Tree is an entrypoint Alphasfile plus every package it
// transitively imports, each file loaded once. The stack is the entrypoint's
// modules, what its imports name, and, repeated until nothing changes, what
// any module or package already in the stack imports.
//
// A module of a package has the id "<package>/<module>"; every other module
// is known by its bare name.
type Tree struct {
	files   []*treeFile // load order, entrypoint first
	byPath  map[string]*treeFile
	modules []*moduleBlock // instantiated modules, in load order, Name set to the id
	edges   []ImportEdge
	unused  []UnusedModule

	// links holds every import per scope: DefaultModule for the
	// entrypoint's top level, a module id, or pkgScope(<package>).
	links map[string][]importLink
	// owners maps every declared module id to its file; scopes maps every
	// scope to the modules and packages it may reference.
	owners map[string]*treeFile
	scopes map[string]*scopeVis
	active map[string]bool

	packages      map[string]*pkgInstance
	outputs       []*output
	inputs        []*input
	entryServices []*serviceBlock
	offs          *offIndex
	chain         map[string]bool
	src           importSource
	changes       []LockChange
	pins          []LockPin
}

// ImportEdge is one imported file and the modules the stack takes from it,
// unioned across every importer that is part of the stack. A package edge
// names the package, the features it runs with, and the packages and
// modules other than the entrypoint's top level that import it.
type ImportEdge struct {
	Path       string
	Modules    []string
	Package    string
	Features   []string
	ImportedBy []string
	// Origin is where a remote file came from: "search <dir>" or
	// "<repo>@<commit>"; empty for a local file.
	Origin string
	// Remote is the identity of the imported file's directory when it was
	// reached through a remote import, directly or by a relative import
	// from such a file; empty otherwise.
	Remote string
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
	release, err := lockPins(opts)
	if err != nil {
		return nil, err
	}
	defer release()
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
		if err := src.lock.write(true); err != nil {
			return nil, fmt.Errorf("write %s: %w", LockFileName, err)
		}
	}
	t.changes = src.changes
	t.pins = src.lock.pins()
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
	f := newTreeFile(abs, abs, src, root)
	if err := t.adopt(f, nil); err != nil {
		return nil, err
	}
	if err := t.finish(); err != nil {
		return nil, err
	}
	return t, nil
}

// Root is the entrypoint's absolute path.
func (t *Tree) Root() string { return t.files[0].path }

// Files lists every loaded file, entrypoint first.
func (t *Tree) Files() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(t.files))
	for _, f := range t.files {
		if !seen[f.path] {
			seen[f.path] = true
			out = append(out, f.path)
		}
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

// Pins lists the repositories this load took from the lock or locked, sorted.
func (t *Tree) Pins() []LockPin { return t.pins }

// Bytes is the manifest identity input for invocation.ConfigHash: the
// entrypoint's bytes, then each loaded file's identity and bytes. A tree
// without imports hashes exactly like the lone entrypoint did before.
func (t *Tree) Bytes() []byte {
	out := append([]byte(nil), t.files[0].src...)
	seen := map[string]bool{t.files[0].path: true}
	for _, f := range t.files[1:] {
		if seen[f.path] {
			continue
		}
		seen[f.path] = true
		out = append(out, 0)
		out = append(out, f.identity...)
		out = append(out, 0)
		out = append(out, f.src...)
	}
	seenParts := map[string]bool{}
	for _, f := range t.files {
		for _, p := range f.parts {
			if seenParts[p.path] {
				continue
			}
			seenParts[p.path] = true
			out = append(out, 0)
			out = append(out, p.path...)
			out = append(out, 0)
			out = append(out, p.src...)
		}
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
	// pkg and block are set for a package's file.
	pkg   *pkgInstance
	block *packageBlock
	// confine, repoAt and origin are set for files of a remote checkout:
	// its root, "<repo>@<commit>", and a short form for display.
	confine string
	repoAt  string
	origin  string
	// remote is ImportEdge.Remote for this file.
	remote string
	// parts are the Alphasfile.<name> files joined to this one.
	parts    []filePart
	disabled []hcl.Range
}

// pkgInstance is a package in the stack, instantiated under its name:
// the alias it was imported with, a scoped "<parent>/<name>", or its package
// block's label.
type pkgInstance struct {
	name string
	file *treeFile
	at   hcl.Range
	// entry is set when the entrypoint imports the package at its top level,
	// or runs it on its own; set is what that import passed, nil for a run
	// on its own. needs are the other imports of it in the stack.
	entry    bool
	set      *pkgSettings
	injected []*pkgSettings
	rules    []*pkgRule
	needs    []pkgNeed
	active   bool
	settings *scopeSettings
	// args are the values the imports in the stack pass to each input, the
	// entrypoint's first; an input no import sets takes its default.
	args map[string][]*inputArg
}

type pkgRule struct {
	key    string
	at     hcl.Range
	target *pkgInstance
	set    *pkgSettings
	used   *bool
}

type pkgSettings struct {
	at       hcl.Range
	inputs   map[string]*inputArg
	features []string
}

// scopeSettings is what a package's modules see as features.<n>, fixed
// before planning.
type scopeSettings struct {
	features map[string]bool
}

// scopeVis is what code in one scope may reference: modules by id and
// packages by local name -> pkgInstance.name.
type scopeVis struct {
	mods map[string]bool
	pkgs map[string]string
}

type importLink struct {
	file      *treeFile
	modules   []string
	pkg       *pkgInstance
	localName string
	bound     bool
	block     *importBlock
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

func pkgScope(name string) string { return "pkg:" + name }

// packageOf returns the package a scope belongs to: the part before the last
// '/' of a package module id, or the name in a pkgScope.
func packageOf(scope string) (string, bool) {
	if p, ok := strings.CutPrefix(scope, "pkg:"); ok {
		return p, true
	}
	if p, _, ok := SplitPackageModule(scope); ok {
		return p, true
	}
	return "", false
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
	}
	f.adoptBlocks()
	return f
}

// adoptBlocks indexes the file's components and points every service at
// the file, parts included.
func (f *treeFile) adoptBlocks() {
	f.declared = map[string]*moduleBlock{}
	for _, mb := range f.root.Modules {
		f.declared[mb.Name] = mb
	}
	for _, sb := range f.root.allServices() {
		sb.file = f
	}
	if len(f.root.Packages) == 1 {
		f.block = f.root.Packages[0]
		for _, mb := range f.block.Modules {
			for _, sb := range mb.Services {
				sb.file = f
			}
		}
	}
}

// adopt registers a loaded file. The entrypoint may be a package's file:
// then the package runs on its own, with default inputs and no features.
func (t *Tree) adopt(f *treeFile, pkg *pkgInstance) error {
	if f.block != nil && pkg == nil {
		pkg = &pkgInstance{name: f.block.Name, at: f.block.DefRange, entry: true}
		t.links[DefaultModule] = append(t.links[DefaultModule], importLink{file: f, pkg: pkg, localName: f.block.Name})
	}
	if pkg != nil {
		if pkg.name == "" {
			pkg.name = f.block.Name
		}
		if prev := t.packages[pkg.name]; prev != nil {
			return fmt.Errorf("%s: another package is already named %q (%s, at %s); import one of them with an alias: import \"<path>\" \"<alias>\" {}", pkg.at, pkg.name, prev.file.dir, prev.at)
		}
		t.packages[pkg.name] = pkg
		pkg.file = f
		f.pkg = pkg
	}
	t.register(f)
	return nil
}

func (t *Tree) register(f *treeFile) {
	if t.byPath[f.path] == nil {
		t.byPath[f.path] = f
	}
	t.files = append(t.files, f)
}

// load reads one file and every file it imports. Imports of scopes that
// never join the stack are loaded and checked too, so a broken file fails
// `zordon plan` before anyone reaches for it.
func (t *Tree) load(path string, importer *treeFile, imp *importBlock, pkg *pkgInstance, res resolved) (*treeFile, error) {
	b, root, err := t.readAndDecode(path, importer, imp, pkg != nil)
	if err != nil {
		return nil, err
	}
	return t.loadDecoded(path, b, root, importer, imp, pkg, res)
}

func (t *Tree) readAndDecode(path string, importer *treeFile, imp *importBlock, wantPkg bool) ([]byte, *rootBlock, error) {
	if wantPkg && t.chain[path] {
		return nil, nil, fmt.Errorf("%s: cannot %s %q: %s is a federation level of this invocation, so its services would run twice", imp.DefRange, imp.keyword, imp.Path, path)
	}
	b, err := zfs.Read(path)
	if err != nil {
		if importer == nil {
			return nil, nil, fmt.Errorf("alphasfile read: %w", err)
		}
		if wantPkg && zfs.IsMissingErr(err) {
			return nil, nil, fmt.Errorf("%s: %s %q: %s has no %s, so it is not a package", imp.DefRange, imp.keyword, imp.Path, filepath.Dir(path), invocation.AlphasfileName)
		}
		return nil, nil, fmt.Errorf("%s: %s %q: %w", imp.DefRange, imp.keyword, imp.Path, err)
	}
	root, err := decodeFile(path, b)
	if err != nil {
		return nil, nil, err
	}
	isPackage := len(root.Packages) > 0
	switch {
	case isPackage:
		if err := checkPackageFile(root); err != nil {
			return nil, nil, err
		}
		if importer != nil && !wantPkg {
			return nil, nil, fmt.Errorf("%s: %s %q: %s is a package's file; %s its directory instead", imp.DefRange, imp.keyword, imp.Path, path, imp.keyword)
		}
	case wantPkg:
		return nil, nil, fmt.Errorf("%s: %s %q: %s has no package block, so it is not a package; wrap its components in package \"<name>\" {}", imp.DefRange, imp.keyword, imp.Path, path)
	}
	return b, root, nil
}

func (t *Tree) loadDecoded(path string, b []byte, root *rootBlock, importer *treeFile, imp *importBlock, pkg *pkgInstance, res resolved) (*treeFile, error) {
	f := newTreeFile(path, path, b, root)
	f.confine, f.repoAt, f.origin = res.confine, res.repoAt, res.origin
	f.remote = remoteDir(importer, imp, pkg != nil, f.dir)
	if err := t.loadParts(f); err != nil {
		return nil, err
	}
	if err := t.adopt(f, pkg); err != nil {
		return nil, err
	}
	if err := t.src.visit(f); err != nil {
		return nil, err
	}
	if f.block != nil {
		name := f.pkg.name
		if err := t.follow(f, pkgScope(name), f.block.Imports); err != nil {
			return nil, err
		}
		for _, mb := range f.block.Modules {
			if err := t.follow(f, name+"/"+mb.Name, mb.Imports); err != nil {
				return nil, err
			}
		}
		return f, nil
	}
	if err := t.follow(f, DefaultModule, root.Imports); err != nil {
		return nil, err
	}
	for _, mb := range root.Modules {
		if err := t.follow(f, mb.Name, mb.Imports); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (t *Tree) follow(f *treeFile, scope string, blocks []*importBlock) error {
	for _, ib := range orderImportBlocks(blocks) {
		ib.keyword = "import"
		if err := checkImportAttrs(f, ib); err != nil {
			return err
		}
		if ok, err := t.followBoundPackage(f, scope, ib); err != nil || ok {
			if err != nil {
				return err
			}
			continue
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
		return fmt.Errorf("%s: %s %q names a file; an import names a package's directory, and an %s.<name> file is read only as a part of the %s next to it", ib.DefRange, ib.keyword, ib.Path, invocation.AlphasfileName, invocation.AlphasfileName)
	}
	return nil
}

func orderImportBlocks(blocks []*importBlock) []*importBlock {
	if len(blocks) <= 1 {
		return blocks
	}
	targets := map[string]bool{}
	hasPackages := false
	for _, ib := range blocks {
		if ib.PackagesRange == (hcl.Range{}) {
			continue
		}
		hasPackages = true
		for _, trav := range ib.Packages.Variables() {
			if len(trav) == 2 && trav.RootName() == "package" {
				if name, ok := traverseAttrName(trav[1]); ok {
					targets[name] = true
				}
			}
		}
	}
	if !hasPackages {
		return blocks
	}
	var pass1, pass2, pass3 []*importBlock
	for _, ib := range blocks {
		cand := ib.alias
		if cand == "" {
			cand = path.Base(ib.Path)
		}
		switch {
		case targets[cand] && ib.PackagesRange == (hcl.Range{}):
			pass1 = append(pass1, ib)
		case ib.PackagesRange != (hcl.Range{}):
			pass2 = append(pass2, ib)
		default:
			pass3 = append(pass3, ib)
		}
	}
	out := make([]*importBlock, 0, len(blocks))
	out = append(out, pass1...)
	out = append(out, pass2...)
	out = append(out, pass3...)
	return out
}

func (t *Tree) followBoundPackage(f *treeFile, scope string, ib *importBlock) (bool, error) {
	if f.pkg == nil || isLocalPath(ib.Path) {
		return false, nil
	}
	cand := ib.alias
	if cand == "" {
		cand = path.Base(ib.Path)
	}
	var boundTarget *pkgInstance
	for _, r := range f.pkg.rules {
		if r.key == cand && r.target != nil {
			*r.used = true
			if boundTarget == nil {
				boundTarget = r.target
			}
		}
	}
	if boundTarget == nil {
		return false, nil
	}
	if ib.Modules != nil {
		return false, fmt.Errorf("%s: %s %q: a package is imported whole; drop components", ib.DefRange, ib.keyword, ib.Path)
	}
	if ib.alias != "" && !moduleNameRe.MatchString(ib.alias) {
		return false, fmt.Errorf("%s: %s %q: %q is not a valid package name", ib.DefRange, ib.keyword, ib.Path, ib.alias)
	}
	ownRules, err := t.parsePackageRules(f, scope, ib)
	if err != nil {
		return false, err
	}
	if len(ownRules) > 0 {
		if err := t.applyRulesToLoaded(boundTarget, ownRules); err != nil {
			return false, err
		}
		for _, r := range ownRules {
			if !*r.used {
				return false, fmt.Errorf("%s: import %q: packages[%q] does not match any package imported by %s", r.at, ib.Path, r.key, boundTarget.name)
			}
		}
	}
	t.links[scope] = append(t.links[scope], importLink{file: boundTarget.file, pkg: boundTarget, localName: cand, bound: true, block: ib})
	return true, nil
}

func (t *Tree) followPackage(f *treeFile, scope string, ib *importBlock, res resolved) error {
	if ib.Modules != nil {
		return fmt.Errorf("%s: %s %q: a package is imported whole; drop components", ib.DefRange, ib.keyword, ib.Path)
	}
	if ib.alias != "" && !moduleNameRe.MatchString(ib.alias) {
		return fmt.Errorf("%s: %s %q: %q is not a valid package name", ib.DefRange, ib.keyword, ib.Path, ib.alias)
	}
	afPath := filepath.Join(res.path, invocation.AlphasfileName)
	if res.confine != "" && !confined(res.confine, afPath) {
		return fmt.Errorf("%s: %s %q: its %s leaves the checkout of %s", ib.DefRange, ib.keyword, ib.Path, invocation.AlphasfileName, res.repoAt)
	}
	if len(t.files) > 0 && afPath == t.files[0].path && t.files[0].pkg == nil {
		return fmt.Errorf("%s: %s %q: %s is the entrypoint, not a package", ib.DefRange, ib.keyword, ib.Path, afPath)
	}
	b, root, err := t.readAndDecode(afPath, f, ib, true)
	if err != nil {
		return err
	}
	declName := root.Packages[0].Name
	localName := ib.alias
	if localName == "" {
		localName = declName
	}
	ownRules, err := t.parsePackageRules(f, scope, ib)
	if err != nil {
		return err
	}
	var boundTarget *pkgInstance
	var injected []*pkgSettings
	var specificRules, inheritedRules []*pkgRule
	if f.pkg != nil {
		for _, r := range f.pkg.rules {
			switch {
			case r.key == localName:
				*r.used = true
				if r.target != nil && boundTarget == nil {
					boundTarget = r.target
				}
				if r.set != nil {
					injected = append(injected, r.set)
				}
			case strings.HasPrefix(r.key, localName+"/"):
				sub := *r
				sub.key = strings.TrimPrefix(r.key, localName+"/")
				specificRules = append(specificRules, &sub)
			case !strings.Contains(r.key, "/") && r.target != nil:
				inheritedRules = append(inheritedRules, r)
			}
		}
	}
	childRules := slices.Concat(specificRules, ownRules, inheritedRules)

	if boundTarget != nil {
		if len(ownRules) > 0 {
			if err := t.applyRulesToLoaded(boundTarget, ownRules); err != nil {
				return err
			}
			for _, r := range ownRules {
				if !*r.used {
					return fmt.Errorf("%s: import %q: packages[%q] does not match any package imported by %s", r.at, ib.Path, r.key, boundTarget.name)
				}
			}
		}
		t.links[scope] = append(t.links[scope], importLink{file: boundTarget.file, pkg: boundTarget, localName: localName, bound: true, block: ib})
		return nil
	}

	pkgName := localName
	if f.pkg != nil {
		switch {
		case len(injected) > 0:
			pkgName = f.pkg.name + "/" + localName
		case hasTargetedRule(childRules) && t.packages[pkgName] != nil:
			pkgName = f.pkg.name + "/" + localName
		case t.packages[pkgName] != nil && t.packages[pkgName].file.path != afPath && t.packages[pkgName].file.block != nil && t.packages[pkgName].file.block.Name == declName && sameRemoteRepo(t.packages[pkgName].file.repoAt, res.repoAt):
			pkgName = f.pkg.name + "/" + localName
		}
	}

	var pkg *pkgInstance
	var tf *treeFile
	if prev := t.packages[pkgName]; prev != nil {
		if prev.file.path != afPath {
			return fmt.Errorf("%s: another package is already named %q (%s, at %s); import one of them with an alias: import \"<path>\" \"<alias>\" {}", ib.DefRange, pkgName, prev.file.dir, prev.at)
		}
		pkg = prev
		tf = prev.file
		if len(injected) > 0 {
			pkg.injected = append(pkg.injected, injected...)
		}
		if len(childRules) > 0 {
			if err := t.applyRulesToLoaded(pkg, childRules); err != nil {
				return err
			}
		}
	} else {
		pkg = &pkgInstance{name: pkgName, at: ib.DefRange, injected: injected, rules: childRules}
		if tf, err = t.loadDecoded(afPath, b, root, f, ib, pkg, res); err != nil {
			return err
		}
		tf.identity = res.identity + "/" + invocation.AlphasfileName
	}

	for _, r := range ownRules {
		if !*r.used {
			return fmt.Errorf("%s: import %q: packages[%q] does not match any package imported by %s", r.at, ib.Path, r.key, pkg.name)
		}
	}

	if scope == DefaultModule {
		set, err := passedSettings(ib, scope, f.srcOf(ib.DefRange.Filename))
		if err != nil {
			return err
		}
		if pkg.set == nil {
			pkg.entry, pkg.set = true, set
		} else if !sameSettings(pkg.set, set) {
			return fmt.Errorf("%s: import %q passes other inputs or features than the import at %s; a package runs once, so every import of it must pass the same", ib.DefRange, ib.Path, pkg.set.at)
		}
	}
	t.links[scope] = append(t.links[scope], importLink{file: tf, pkg: pkg, localName: localName, block: ib})
	return nil
}

func hasTargetedRule(rules []*pkgRule) bool {
	for _, r := range rules {
		if r.set != nil || strings.Contains(r.key, "/") {
			return true
		}
	}
	return false
}

func sameRemoteRepo(a, b string) bool {
	ra, _, okA := strings.Cut(a, "@")
	rb, _, okB := strings.Cut(b, "@")
	return okA && okB && ra == rb && a != b
}

func (t *Tree) applyRulesToLoaded(pkg *pkgInstance, rules []*pkgRule) error {
	scopes := []string{pkgScope(pkg.name)}
	if pkg.file != nil && pkg.file.block != nil {
		for _, mb := range pkg.file.block.Modules {
			scopes = append(scopes, pkg.name+"/"+mb.Name)
		}
	}
	for _, sc := range scopes {
		links := t.links[sc]
		for i := range links {
			l := &links[i]
			if l.pkg == nil {
				continue
			}
			matchedTarget := false
			var specificRules, inheritedRules []*pkgRule
			for _, r := range rules {
				switch {
				case r.key == l.localName:
					*r.used = true
					if r.target != nil && !matchedTarget {
						matchedTarget = true
						l.pkg = r.target
						l.file = r.target.file
						l.bound = true
					}
					if r.set != nil {
						l.pkg.injected = append([]*pkgSettings{r.set}, l.pkg.injected...)
					}
				case strings.HasPrefix(r.key, l.localName+"/"):
					sub := *r
					sub.key = strings.TrimPrefix(r.key, l.localName+"/")
					specificRules = append(specificRules, &sub)
				case !strings.Contains(r.key, "/") && r.target != nil:
					inheritedRules = append(inheritedRules, r)
				}
			}
			subRules := slices.Concat(specificRules, inheritedRules)
			if len(subRules) > 0 {
				if err := t.applyRulesToLoaded(l.pkg, subRules); err != nil {
					return err
				}
			}
		}
	}
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
				return fmt.Errorf("duplicate component %q: declared at %s and %s", mb.Name, prev.declared[mb.Name].DefRange, mb.DefRange)
			}
			t.owners[mb.Name] = f
		}
		if f.block != nil {
			for _, mb := range f.block.Modules {
				t.owners[f.pkg.name+"/"+mb.Name] = f
			}
		}
	}
	if err := t.checkPackageNames(); err != nil {
		return err
	}

	if err := t.resolvePackages(); err != nil {
		return err
	}
	var err error

	// Every module of the entrypoint is in the stack, so the entrypoint's
	// scopes see all of them, whichever part declares them. A package's modules
	// see each other and whatever the package or the module imports.
	entry := t.files[0]
	t.scopes = map[string]*scopeVis{}
	for _, f := range t.files {
		if f.block != nil {
			p := f.pkg.name
			pv := newScopeVis()
			for _, mb := range f.block.Modules {
				pv.mods[p+"/"+mb.Name] = true
			}
			if err := t.linked(pv, pkgScope(p)); err != nil {
				return err
			}
			t.scopes[pkgScope(p)] = pv
			for _, mb := range f.block.Modules {
				mv := pv.clone()
				if err := t.linked(mv, p+"/"+mb.Name); err != nil {
					return err
				}
				t.scopes[p+"/"+mb.Name] = mv
			}
			continue
		}
		for _, mb := range f.root.Modules {
			vis := newScopeVis()
			vis.mods[mb.Name] = true
			if f == entry {
				for name := range entry.declared {
					vis.mods[name] = true
				}
			}
			if err := t.linked(vis, mb.Name); err != nil {
				return err
			}
			t.scopes[mb.Name] = vis
		}
	}
	top := newScopeVis()
	for name := range entry.declared {
		top.mods[name] = true
	}
	if err := t.linked(top, DefaultModule); err != nil {
		return err
	}
	t.scopes[DefaultModule] = top

	t.active = map[string]bool{}
	queue := []string{DefaultModule}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if t.active[node] {
			continue
		}
		t.active[node] = true
		if vis := t.scopes[node]; vis != nil {
			queue = append(queue, sortedKeys(vis.mods)...)
			for _, local := range sortedKeys(vis.pkgs) {
				queue = append(queue, pkgScope(vis.pkgs[local]))
			}
		}
	}

	toolchains, err := t.packageToolchains()
	if err != nil {
		return err
	}
	for _, f := range t.files {
		if f.block != nil {
			p := f.pkg.name
			if !t.active[pkgScope(p)] {
				t.unused = append(t.unused, UnusedModule{Path: f.path, Module: "package " + p})
				continue
			}
			t.addEdges(pkgScope(p))
			for _, mb := range f.block.Modules {
				id := p + "/" + mb.Name
				cp, err := t.pruneModule(mb, f.pkg.settings, id, toolchains[p].toolchainBlock())
				if err != nil {
					return err
				}
				t.modules = append(t.modules, cp)
				t.addEdges(id)
			}
			if f == entry {
				t.addEdges(DefaultModule)
			}
			continue
		}
		if f == entry {
			if t.entryServices, err = t.prune(entry.root.Services, nil, DefaultModule); err != nil {
				return err
			}
			t.addEdges(DefaultModule)
		}
		for _, mb := range f.root.Modules {
			if !t.active[mb.Name] {
				t.unused = append(t.unused, UnusedModule{Path: f.path, Module: mb.Name})
				continue
			}
			cp, err := t.pruneModule(mb, nil, mb.Name, nil)
			if err != nil {
				return err
			}
			t.modules = append(t.modules, cp)
			t.addEdges(mb.Name)
		}
	}
	t.gatherValues()
	return t.checkGated()
}

// checkPackageNames reports a package whose services' state directories and
// branches would nest inside another service's: a top-level service named
// like the package, or a module named like it with a service named like one
// of the package's modules.
func (t *Tree) checkPackageNames() error {
	const fix = "both would own <state>/{bin,src,etc,var}/%s, so import it with an alias: import \"<path>\" \"<alias>\" {}"
	for _, name := range sortedKeys(t.packages) {
		p := t.packages[name]
		if owner := t.owners[name]; owner != nil && p.file.block != nil {
			mb := owner.declared[name]
			for _, sb := range mb.Services {
				for _, pm := range p.file.block.Modules {
					if pm.Name == sb.Name {
						return fmt.Errorf("%s: package %q has component %q, and component %q declared at %s has service %q; "+fix, p.at, name, pm.Name, name, mb.DefRange, sb.Name, name+"/"+sb.Name)
					}
				}
			}
		}
		for _, sb := range t.files[0].root.Services {
			if sb.Name == name {
				return fmt.Errorf("%s: package %q has the same name as the top-level service declared at %s; "+fix, p.at, name, sb.DefRange, name)
			}
		}
	}
	return nil
}

func newScopeVis() *scopeVis {
	return &scopeVis{mods: map[string]bool{}, pkgs: map[string]string{}}
}

func (v *scopeVis) clone() *scopeVis {
	c := newScopeVis()
	for k := range v.mods {
		c.mods[k] = true
	}
	maps.Copy(c.pkgs, v.pkgs)
	return c
}

// linked adds every module and package the scope imports to vis, skipping
// imports switched off by enabled.
func (t *Tree) linked(vis *scopeVis, scope string) error {
	for _, l := range t.links[scope] {
		on, err := t.linkEnabled(scope, l)
		if err != nil {
			return err
		}
		local := l.localName
		if local == "" && l.pkg != nil {
			local = l.pkg.name
		}
		if !on {
			if t.off().links[scope] == nil {
				t.off().links[scope] = map[string]offRecord{}
			}
			if l.pkg != nil {
				t.off().links[scope][pkgScope(local)] = offRecord{what: "package." + local, at: l.block.EnabledRange}
			}
			for _, m := range l.modules {
				t.off().links[scope][m] = offRecord{what: "component." + m, at: l.block.EnabledRange}
			}
			continue
		}
		if l.pkg != nil {
			vis.pkgs[local] = l.pkg.name
		}
		for _, m := range l.modules {
			vis.mods[m] = true
		}
	}
	return nil
}

func (t *Tree) linkEnabled(scope string, l importLink) (bool, error) {
	if l.block == nil || l.block.EnabledRange == (hcl.Range{}) {
		return true, nil
	}
	return evalEnabled(l.block.Enabled, t.settingsFor(scope))
}

func (t *Tree) addEdges(scope string) {
	for _, l := range t.links[scope] {
		if on, _ := t.linkEnabled(scope, l); !on {
			continue
		}
		if l.pkg != nil && l.file == t.files[0] {
			continue
		}
		var features, by []string
		pkgName := ""
		if l.pkg != nil {
			pkgName = l.pkg.name
			for n, on := range l.pkg.settings.features {
				if on {
					features = append(features, n)
				}
			}
			sort.Strings(features)
			for _, n := range l.pkg.needs {
				if !slices.Contains(by, n.by) {
					by = append(by, n.by)
				}
			}
		}
		t.addEdge(l.file, l.modules, pkgName, features, by)
	}
}

func (t *Tree) addEdge(f *treeFile, modules []string, pkg string, features, by []string) {
	path := f.path
	for i := range t.edges {
		if t.edges[i].Path != path || t.edges[i].Package != pkg {
			continue
		}
		for _, m := range modules {
			if !containsString(t.edges[i].Modules, m) {
				t.edges[i].Modules = append(t.edges[i].Modules, m)
			}
		}
		return
	}
	t.edges = append(t.edges, ImportEdge{Path: path, Modules: append([]string(nil), modules...), Package: pkg, Features: features, ImportedBy: by, Origin: f.origin, Remote: f.remote})
}

// remoteDir is the identity dir is known by when a file in it was reached
// through a remote import: the import's path, or the importer's identity
// joined with the relative path it imported.
func remoteDir(importer *treeFile, imp *importBlock, isPackage bool, dir string) string {
	switch {
	case imp == nil:
		return ""
	case !isLocalPath(imp.Path) && isPackage:
		return imp.Path
	case !isLocalPath(imp.Path):
		return path.Dir(imp.Path)
	case importer == nil || importer.remote == "" || !strings.HasPrefix(imp.Path, "."):
		return ""
	}
	rel, err := filepath.Rel(importer.dir, dir)
	if err != nil {
		return ""
	}
	return path.Join(importer.remote, filepath.ToSlash(rel))
}

// settingsFor returns the inputs and features a scope sees: its package's,
// or nil outside packages.
func (t *Tree) settingsFor(scope string) *scopeSettings {
	if p, ok := packageOf(scope); ok {
		if pkg := t.packages[p]; pkg != nil {
			return pkg.settings
		}
	}
	return nil
}

// resolveModule maps `component.<c>` written in scope to a module id: a
// sibling "<package>/<m>" inside a package, the bare name elsewhere.
func resolveModule(scope, m string) string {
	if p, ok := packageOf(scope); ok {
		return p + "/" + m
	}
	return m
}

// resolvePackageRef maps `package.<p>` written in scope to the pkgInstance
// name bound to <p> in that scope.
func (t *Tree) resolvePackageRef(scope, name string) string {
	if t != nil {
		if vis := t.scopes[scope]; vis != nil {
			if target, ok := vis.pkgs[name]; ok {
				return target
			}
		}
		if p, ok := packageOf(scope); ok {
			if vis := t.scopes[pkgScope(p)]; vis != nil {
				if target, ok := vis.pkgs[name]; ok {
					return target
				}
			}
		}
	}
	return name
}

// moduleVisible reports whether code in scope may reference module id. An
// id no file of this tree declares, such as a federation parent's module,
// is not the tree's to hide.
func (t *Tree) moduleVisible(scope, id string) bool {
	if t.owners[id] == nil {
		return true
	}
	vis := t.scopes[scope]
	return vis != nil && vis.mods[id]
}

// packageVisible reports whether code in scope may reference package p.
func (t *Tree) packageVisible(scope, p string) bool {
	resolved := t.resolvePackageRef(scope, p)
	if t.packages[resolved] == nil {
		return true
	}
	vis := t.scopes[scope]
	if vis == nil {
		return false
	}
	if vis.pkgs[p] != "" {
		return true
	}
	for _, target := range vis.pkgs {
		if target == p {
			return true
		}
	}
	return false
}

func (f *treeFile) isDisabled(r hcl.Range) bool {
	if f == nil {
		return false
	}
	for _, d := range f.disabled {
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
	rb.Packages = nil
	return &rb
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
	for _, pb := range root.Packages {
		if err := pb.decodeAPI(); err != nil {
			return nil, err
		}
	}
	return &root, nil
}

// liftAliases moves the optional second label of import blocks (the alias)
// into aliases, keyed by the block's start offset, so gohcl sees one label.
func liftAliases(body *hclsyntax.Body, aliases map[int]string) {
	for _, blk := range body.Blocks {
		switch blk.Type {
		case "import":
			if len(blk.Labels) == 2 {
				aliases[blk.TypeRange.Start.Byte] = blk.Labels[1]
				blk.Labels = blk.Labels[:1]
				blk.LabelRanges = blk.LabelRanges[:1]
			}
		case "component", "package":
			liftAliases(blk.Body, aliases)
		}
	}
}

// checkPackageFile enforces that a package's file is one package block and
// nothing else, so nothing that decides for the stack around it fits in.
func checkPackageFile(root *rootBlock) error {
	if len(root.Packages) > 1 {
		return fmt.Errorf("%s: a file holds one package block; %q is already declared at %s", root.Packages[1].DefRange, root.Packages[0].Name, root.Packages[0].DefRange)
	}
	outside := fmt.Sprintf("outside package %q; a package's file holds only its package block", root.Packages[0].Name)
	switch {
	case len(root.Services) > 0:
		return fmt.Errorf("%s: service %s", root.Services[0].DefRange, outside)
	case len(root.Modules) > 0:
		return fmt.Errorf("%s: component %q %s", root.Modules[0].DefRange, root.Modules[0].Name, outside)
	case len(root.Imports) > 0:
		return fmt.Errorf("%s: import %q %s", root.Imports[0].DefRange, root.Imports[0].Path, outside)
	case len(root.Requires) > 0:
		return fmt.Errorf("%s: require %q %s", root.Requires[0].DefRange, root.Requires[0].Repo, outside)
	case root.Toolchain != nil:
		return fmt.Errorf("%s: toolchain %s", root.Toolchain.DefRange, outside)
	case root.Workspace != nil:
		return fmt.Errorf("%s: workspace %s", root.Workspace.DefRange, outside)
	case root.EnvRange != (hcl.Range{}):
		return fmt.Errorf("%s: env %s", root.EnvRange, outside)
	case root.DotenvRange != (hcl.Range{}):
		return fmt.Errorf("%s: dotenv %s", root.DotenvRange, outside)
	case root.SysEnvRange != (hcl.Range{}):
		return fmt.Errorf("%s: sysenv %s; host variables are passed by the entrypoint", root.SysEnvRange, outside)
	}
	return nil
}

func checkImportAttrs(f *treeFile, ib *importBlock) error {
	if ib.EnabledRange == (hcl.Range{}) {
		return nil
	}
	if f.block == nil || len(f.block.features) == 0 {
		return fmt.Errorf("%s: import %q: enabled needs a feature, and none is declared here; only a package declares features = { ... }", ib.EnabledRange, ib.Path)
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
