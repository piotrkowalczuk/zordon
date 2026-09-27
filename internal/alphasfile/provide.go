package alphasfile

import (
	"fmt"
	"slices"

	"github.com/hashicorp/hcl/v2"
)

// provision is one entry an import in the stack provides to a slot of the
// package it imports. Its attributes are evaluated in scope, the scope of
// the import, so they reference what the importer sees.
type provision struct {
	pkg   string
	slot  string
	key   string
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
			collects := l.pkg.file.block.collects
			for _, pb := range l.block.Provides {
				if _, ok := collects[pb.Slot]; !ok {
					return fmt.Errorf("%s: provide %q %q: package %s does not collect %q (collects: %s)", pb.DefRange, pb.Slot, pb.Key, l.pkg.name, pb.Slot, listOrNone(sortedKeys(collects)))
				}
				if !moduleNameRe.MatchString(pb.Key) {
					return fmt.Errorf("%s: provide %q %q: use letters, digits, '_' or '-' for the key and start with a letter", pb.DefRange, pb.Slot, pb.Key)
				}
				attrs, diags := pb.Body.JustAttributes()
				if diags.HasErrors() {
					return fmt.Errorf("%s: provide %q %q takes attributes only: %s", pb.DefRange, pb.Slot, pb.Key, diags.Error())
				}
				p := &provision{pkg: l.pkg.name, slot: pb.Slot, key: pb.Key, scope: scope, block: pb, attrs: attrs}
				if prev, dup := seen[p.id()]; dup {
					return fmt.Errorf("%s: provide %q %q to package %s is already provided at %s; each key has one source", pb.DefRange, pb.Slot, pb.Key, l.pkg.name, prev.block.DefRange)
				}
				seen[p.id()] = p
				t.provisions = append(t.provisions, p)
			}
		}
	}
	slices.SortFunc(t.provisions, func(a, b *provision) int {
		switch {
		case a.id() < b.id():
			return -1
		case a.id() > b.id():
			return 1
		}
		return 0
	})
	return nil
}
