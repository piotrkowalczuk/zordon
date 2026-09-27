package alphasfile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// provision is one entry an import in the stack provides to an input with
// many = true of the package it imports. Its attributes are evaluated in
// scope, the scope of the import, so they reference what the importer sees.
// key is the entry's full key: the provider's name, then the provide block's
// own key, if any.
type provision struct {
	pkg   string
	input string
	key   string
	decl  *inputDecl
	scope string
	block *provideBlock
	attrs hcl.Attributes
}

func (p *provision) id() string { return "provide." + p.pkg + "." + p.input + "." + p.key }

// output is one output of a package in the stack, evaluated in the
// package's scope.
type output struct {
	pkg  string
	name string
	decl *outputDecl
}

func (o *output) id() string { return "output." + o.pkg + "." + o.name }

// input is one single input of a package in the stack: the import that sets
// it, or its default, evaluated with the producers.
type input struct {
	pkg  string
	name string
	decl *inputDecl
	arg  *inputArg
}

func (in *input) id() string { return "input." + in.pkg + "." + in.name }

// scope is where the value's expression is evaluated: the import that sets
// it, or the package itself for a default.
func (in *input) scope() string {
	if in.arg != nil {
		return in.arg.scope
	}
	return pkgScope(in.pkg)
}

func (in *input) expr() hcl.Expression {
	if in.arg != nil {
		return in.arg.expr
	}
	return in.decl.def
}

// check compares another import's value for an input with the value the
// package runs with.
type check struct {
	in *input
	c  *inputCheck
	i  int
}

func (c *check) id() string { return fmt.Sprintf("inputcheck.%s.%s.%d", c.in.pkg, c.in.name, c.i) }

// gatherProvisions collects the provide blocks of every import that is part
// of the stack. An import switched off by enabled provides nothing.
func (t *Tree) gatherProvisions() error {
	seen := map[string]*provision{}
	for _, scope := range sortedKeys(t.links) {
		if !t.active[scope] {
			continue
		}
		for _, l := range t.links[scope] {
			if l.pkg == nil || l.block == nil || len(l.block.Provides) == 0 {
				continue
			}
			if on, err := t.linkEnabled(scope, l); err != nil || !on {
				if err != nil {
					return err
				}
				continue
			}
			for _, pb := range l.block.Provides {
				p, err := newProvision(l.pkg, scope, pb)
				if err != nil {
					return err
				}
				if prev, dup := seen[p.id()]; dup {
					return fmt.Errorf("%s: entry %q of input %q in package %s is already provided at %s; give each entry its own key: provide %q \"<key>\" {}", pb.DefRange, p.key, pb.Slot, l.pkg.name, prev.block.DefRange, pb.Slot)
				}
				seen[p.id()] = p
				t.provisions = append(t.provisions, p)
			}
		}
	}
	slices.SortFunc(t.provisions, func(a, b *provision) int { return strings.Compare(a.id(), b.id()) })
	return nil
}

// gatherValues lists the single inputs, their checks and the outputs of
// every package in the stack.
func (t *Tree) gatherValues() {
	for _, name := range sortedKeys(t.packages) {
		p := t.packages[name]
		if !t.active[pkgScope(name)] {
			continue
		}
		pb := p.file.block
		byName := map[string]*input{}
		for _, n := range sortedKeys(pb.inputs) {
			if decl := pb.inputs[n]; !decl.many {
				in := &input{pkg: name, name: n, decl: decl, arg: p.inputs[n]}
				byName[n] = in
				t.inputs = append(t.inputs, in)
			}
		}
		for i, c := range p.checks {
			t.checks = append(t.checks, &check{in: byName[c.name], c: c, i: i})
		}
		for _, o := range sortedKeys(pb.outputs) {
			t.outputs = append(t.outputs, &output{pkg: name, name: o, decl: pb.outputs[o]})
		}
	}
}

func newProvision(target *pkgInstance, scope string, pb *provideBlock) (*provision, error) {
	inputs := target.file.block.inputs
	decl, ok := inputs[pb.Slot]
	switch {
	case !ok:
		return nil, fmt.Errorf("%s: provide %q: package %s has no input %q (inputs with many = true: %s)", pb.DefRange, pb.Slot, target.name, pb.Slot, listOrNone(manyInputs(inputs)))
	case !decl.many:
		return nil, fmt.Errorf("%s: provide %q: input %q of package %s takes one value; set it with inputs = { %s = ... }", pb.DefRange, pb.Slot, pb.Slot, target.name, pb.Slot)
	}
	if pb.key != "" && !moduleNameRe.MatchString(pb.key) {
		return nil, fmt.Errorf("%s: provide %q %q: use letters, digits, '_' or '-' for the key and start with a letter", pb.DefRange, pb.Slot, pb.key)
	}
	key := providerName(scope)
	switch {
	case key == "" && pb.key == "":
		return nil, fmt.Errorf("%s: provide %q at the entrypoint's top level needs a key: provide %q \"<key>\" {}", pb.DefRange, pb.Slot, pb.Slot)
	case key == "":
		key = pb.key
	case pb.key != "":
		key += "." + pb.key
	}
	attrs, diags := pb.Body.JustAttributes()
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s: provide %q takes attributes only: %s", pb.DefRange, pb.Slot, diags.Error())
	}
	for _, name := range sortedKeys(attrs) {
		if !decl.ty.HasAttribute(name) {
			return nil, fmt.Errorf("%s: provide %q: input %q of package %s has no attribute %q (%s)", attrs[name].NameRange, pb.Slot, pb.Slot, target.name, name, decl.ty)
		}
	}
	for _, name := range decl.ty.Attributes() {
		if _, set := attrs[name]; !set && !decl.ty.Optional(name) {
			return nil, fmt.Errorf("%s: provide %q: input %q of package %s needs attribute %q (%s)", pb.DefRange, pb.Slot, pb.Slot, target.name, name, decl.ty)
		}
	}
	return &provision{pkg: target.name, input: pb.Slot, key: key, decl: decl, scope: scope, block: pb, attrs: attrs}, nil
}

func manyInputs(inputs map[string]*inputDecl) []string {
	var out []string
	for _, n := range sortedKeys(inputs) {
		if inputs[n].many {
			out = append(out, n)
		}
	}
	return out
}

// providerName names who provides from scope: the package, or the module
// outside packages; empty at the entrypoint's top level.
func providerName(scope string) string {
	if p, ok := packageOf(scope); ok {
		return p
	}
	if scope == DefaultModule {
		return ""
	}
	return scope
}
