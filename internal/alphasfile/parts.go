package alphasfile

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// filePart is an Alphasfile.<name> joined to the Alphasfile next to it.
type filePart struct {
	path string
	src  []byte
}

// loadParts joins every Alphasfile.<name> in f's directory to f: the unit
// is their union. A part obeys the rules of the Alphasfile it joins, so
// next to a package's file it opens the same package, and next to an
// entrypoint it holds no package block. Without an Alphasfile a part is
// never read.
func (t *Tree) loadParts(f *treeFile) error {
	entries, err := zfs.ReadDir(f.dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", f.dir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), invocation.AlphasfileName+".") && !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(f.dir, name)
		if f.confine != "" && !confined(f.confine, path) {
			return fmt.Errorf("%s leaves the checkout of %s", path, f.repoAt)
		}
		b, err := zfs.Read(path)
		if err != nil {
			return fmt.Errorf("alphasfile read: %w", err)
		}
		part, err := decodeFile(path, b)
		if err != nil {
			return err
		}
		if err := checkPart(f, part, path); err != nil {
			return err
		}
		if err := mergeRoot(f.root, part); err != nil {
			return err
		}
		f.parts = append(f.parts, filePart{path: path, src: b})
	}
	if len(names) == 0 {
		return nil
	}
	if err := annotateModules(f.root); err != nil {
		return err
	}
	f.adoptBlocks()
	return nil
}

func checkPart(f *treeFile, part *rootBlock, path string) error {
	if f.block == nil {
		if len(part.Packages) > 0 {
			return fmt.Errorf("%s: package %q in %s, a part of the entrypoint %s; a package lives in a directory of its own", part.Packages[0].DefRange, part.Packages[0].Name, path, f.path)
		}
		return nil
	}
	if len(part.Packages) == 0 {
		return fmt.Errorf("%s: a part of package %q (%s) must open it with package %q {} like its %s does", path, f.block.Name, f.path, f.block.Name, invocation.AlphasfileName)
	}
	if err := checkPackageFile(part); err != nil {
		return err
	}
	if got := part.Packages[0].Name; got != f.block.Name {
		return fmt.Errorf("%s: package %q, but %s next to it declares package %q; every part of a directory opens the same package", part.Packages[0].DefRange, got, f.path, f.block.Name)
	}
	return nil
}

// mergeRoot adds part to dst. Lists add up; a name declared twice is an
// error naming both places; env, dotenv and sysenv of every part are
// evaluated and joined with the level's own.
func mergeRoot(dst, part *rootBlock) error {
	dst.Services = append(dst.Services, part.Services...)
	dst.Modules = append(dst.Modules, part.Modules...)
	dst.Imports = append(dst.Imports, part.Imports...)
	dst.Requires = append(dst.Requires, part.Requires...)
	if err := mergeToolchain(&dst.Toolchain, part.Toolchain); err != nil {
		return err
	}
	switch {
	case part.Workspace == nil:
	case dst.Workspace != nil:
		return fmt.Errorf("%s: workspace is already declared at %s; a unit declares it once", part.Workspace.DefRange, dst.Workspace.DefRange)
	default:
		dst.Workspace = part.Workspace
	}
	if part.EnvRange != (hcl.Range{}) || part.DotenvRange != (hcl.Range{}) || part.SysEnvRange != (hcl.Range{}) {
		dst.parts = append(dst.parts, part)
	}
	if len(dst.Packages) == 1 && len(part.Packages) == 1 {
		return mergePackage(dst.Packages[0], part.Packages[0])
	}
	return nil
}

func mergePackage(dst, part *packageBlock) error {
	for name, desc := range part.features {
		if _, dup := dst.features[name]; dup {
			return fmt.Errorf("%s: feature %q of package %q is already declared in another part (%s)", part.DefRange, name, dst.Name, dst.DefRange.Filename)
		}
		dst.features[name] = desc
	}
	for name, decl := range part.inputs {
		if _, dup := dst.inputs[name]; dup {
			return fmt.Errorf("%s: input %q of package %q is already declared in another part (%s)", part.DefRange, name, dst.Name, dst.DefRange.Filename)
		}
		dst.inputs[name] = decl
	}
	for name, decl := range part.outputs {
		if _, dup := dst.outputs[name]; dup {
			return fmt.Errorf("%s: output %q of package %q is already declared in another part (%s)", part.DefRange, name, dst.Name, dst.DefRange.Filename)
		}
		dst.outputs[name] = decl
	}
	if dst.InputsRange == (hcl.Range{}) {
		dst.InputsRange = part.InputsRange
	}
	dst.Imports = append(dst.Imports, part.Imports...)
	dst.Requires = append(dst.Requires, part.Requires...)
	dst.Modules = append(dst.Modules, part.Modules...)
	return mergeToolchain(&dst.Toolchain, part.Toolchain)
}

// mergeToolchain adds the languages part pins to dst; a language pinned by
// two parts is an error.
func mergeToolchain(dst **toolchainBlock, part *toolchainBlock) error {
	if part == nil {
		return nil
	}
	if *dst == nil {
		*dst = part
		return nil
	}
	d := *dst
	dup := func(lang string) error {
		return fmt.Errorf("%s: toolchain %s is already pinned at %s", part.DefRange, lang, d.DefRange)
	}
	for _, l := range []struct {
		name     string
		dst, src **langToolchainBlock
	}{{"go", &d.Go, &part.Go}, {"rust", &d.Rust, &part.Rust}, {"ruby", &d.Ruby, &part.Ruby}, {"nodejs", &d.Nodejs, &part.Nodejs}, {"java", &d.Java, &part.Java}} {
		if *l.src == nil {
			continue
		}
		if *l.dst != nil {
			return dup(l.name)
		}
		*l.dst = *l.src
	}
	if part.Pkg != nil {
		if d.Pkg != nil {
			return dup("pkg")
		}
		d.Pkg = part.Pkg
	}
	return nil
}

// srcOf is the source of the file a range points into: the Alphasfile or
// one of its parts.
func (f *treeFile) srcOf(filename string) []byte {
	for _, p := range f.parts {
		if p.path == filename {
			return p.src
		}
	}
	return f.src
}

// levelParts is the level's root block and every part that sets env,
// dotenv or sysenv, in file order.
func (rb *rootBlock) levelParts() []*rootBlock {
	return append([]*rootBlock{rb}, rb.parts...)
}
