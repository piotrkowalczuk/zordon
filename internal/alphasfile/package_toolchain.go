package alphasfile

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// packageToolchain is the toolchain a package's components run with when
// they pin none of their own, and the package that declares it.
type packageToolchain struct {
	block *toolchainBlock
	file  *treeFile
	from  string
}

// packageToolchains resolves the toolchain of every package in the stack. A
// package with a toolchain {} of its own runs with that block, whole: its
// importers add nothing to it. A package without one runs with the toolchain
// of the packages that import it, which must agree; importers that pin
// different toolchains are an error, settled by a toolchain {} in the package
// itself. Imports from the entrypoint's top level count for nothing, since
// the entrypoint's own pins already apply to the whole level. The toolchain
// of a package flows on to what it imports, so a package that imports a whole
// module pins the module, and packages that import each other settle by
// iterating until nothing changes.
func (t *Tree) packageToolchains() (map[string]*packageToolchain, error) {
	out := map[string]*packageToolchain{}
	for name, p := range t.packages {
		if p.active && p.file.block.Toolchain != nil {
			out[name] = &packageToolchain{block: p.file.block.Toolchain, file: p.file, from: name}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, name := range sortedKeys(t.packages) {
			p := t.packages[name]
			if !p.active || p.file.block.Toolchain != nil {
				continue
			}
			var got *packageToolchain
			var gotBy string
			for _, n := range p.needs {
				by, ok := strings.CutPrefix(n.by, "package ")
				if !ok || out[by] == nil {
					continue
				}
				cand := out[by]
				if got == nil {
					got, gotBy = cand, by
					continue
				}
				if cand.key() != got.key() {
					return nil, fmt.Errorf("package %s: package %s runs it with the toolchain of package %s (%s), but package %s with the toolchain of package %s (%s); give package %s a toolchain {} of its own, or make them agree",
						name, gotBy, got.from, got.block.DefRange, by, cand.from, cand.block.DefRange, name)
				}
			}
			if got != nil && (out[name] == nil || out[name].key() != got.key()) {
				out[name] = got
				changed = true
			}
		}
	}
	return out, nil
}

// toolchainBlock is the block the package's components fall back to; nil
// when no package pins one.
func (pt *packageToolchain) toolchainBlock() *toolchainBlock {
	if pt == nil {
		return nil
	}
	return pt.block
}

// key is the block as written, so two packages that pin the same versions in
// separate blocks agree.
func (pt *packageToolchain) key() string {
	var b strings.Builder
	langs := pt.block.byLabel()
	for _, lang := range []string{ToolchainGo, ToolchainRust, ToolchainRuby, ToolchainNode, ToolchainJava} {
		l := langs[lang]
		if l == nil {
			continue
		}
		fmt.Fprintf(&b, "%s=%s|%s|%s;", lang, l.Version, pt.text(l.Tools), pt.text(l.Env))
	}
	if pt.block.Pkg != nil {
		fmt.Fprintf(&b, "%s=%s;", ToolchainPkg, pt.text(pt.block.Pkg.Tools))
	}
	return b.String()
}

// text is an expression's source with whitespace collapsed; its range when
// the source is not at hand, which keeps blocks from different files apart.
func (pt *packageToolchain) text(e hcl.Expression) string {
	if e == nil {
		return ""
	}
	r := e.Range()
	for _, src := range pt.sources(r.Filename) {
		if r.End.Byte <= len(src) && r.Start.Byte <= r.End.Byte {
			return strings.Join(strings.Fields(string(r.SliceBytes(src))), " ")
		}
	}
	return r.String()
}

func (pt *packageToolchain) sources(filename string) [][]byte {
	if filename == pt.file.path {
		return [][]byte{pt.file.src}
	}
	for _, part := range pt.file.parts {
		if part.path == filename {
			return [][]byte{part.src}
		}
	}
	return nil
}
