package alphasfile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// provision is one entry an import in the stack provides to a slot of the
// package it imports. Its attributes are evaluated in scope, the scope of
// the import, so they reference what the importer sees. key is the entry's
// full key: the provider's name, then the provide block's own key, if any.
type provision struct {
	pkg   string
	slot  string
	key   string
	decl  *slotDecl
	scope string
	block *provideBlock
	attrs hcl.Attributes
}

func (p *provision) id() string { return "provide." + p.pkg + "." + p.slot + "." + p.key }

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
					return fmt.Errorf("%s: entry %q of slot %q in package %s is already provided at %s; give each entry its own key: provide %q \"<key>\" {}", pb.DefRange, p.key, pb.Slot, l.pkg.name, prev.block.DefRange, pb.Slot)
				}
				seen[p.id()] = p
				t.provisions = append(t.provisions, p)
			}
		}
	}
	slices.SortFunc(t.provisions, func(a, b *provision) int { return strings.Compare(a.id(), b.id()) })
	return nil
}

// output is one output of a package in the stack, evaluated in the
// package's scope.
type output struct {
	pkg  string
	name string
	expr hcl.Expression
}

func (o *output) id() string { return "output." + o.pkg + "." + o.name }

// gatherOutputs lists the outputs of every package in the stack.
func (t *Tree) gatherOutputs() {
	for _, name := range sortedKeys(t.packages) {
		p := t.packages[name]
		if !t.active[pkgScope(name)] {
			continue
		}
		for _, o := range sortedKeys(p.file.block.outputs) {
			t.outputs = append(t.outputs, &output{pkg: name, name: o, expr: p.file.block.outputs[o]})
		}
	}
}

func newProvision(target *pkgInstance, scope string, pb *provideBlock) (*provision, error) {
	slots := target.file.block.slots
	decl, ok := slots[pb.Slot]
	if !ok {
		return nil, fmt.Errorf("%s: provide %q: package %s has no slot %q (slots: %s)", pb.DefRange, pb.Slot, target.name, pb.Slot, listOrNone(sortedKeys(slots)))
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
		if !decl.entry.HasAttribute(name) {
			return nil, fmt.Errorf("%s: provide %q: slot %q of package %s has no attribute %q (entry: %s)", attrs[name].NameRange, pb.Slot, pb.Slot, target.name, name, entryAttrs(decl))
		}
	}
	for _, name := range sortedKeys(decl.entry.AttributeTypes()) {
		if _, set := attrs[name]; !set && !decl.entry.AttributeOptional(name) {
			return nil, fmt.Errorf("%s: provide %q: slot %q of package %s needs attribute %q (entry: %s)", pb.DefRange, pb.Slot, pb.Slot, target.name, name, entryAttrs(decl))
		}
	}
	return &provision{pkg: target.name, slot: pb.Slot, key: key, decl: decl, scope: scope, block: pb, attrs: attrs}, nil
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

func entryAttrs(decl *slotDecl) string {
	names := sortedKeys(decl.entry.AttributeTypes())
	for i, n := range names {
		if decl.entry.AttributeOptional(n) {
			names[i] = n + " (optional)"
		}
	}
	return strings.Join(names, ", ")
}
