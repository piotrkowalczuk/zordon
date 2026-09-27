package alphasfile

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// output is one output of a package in the stack, evaluated in the
// package's scope.
type output struct {
	pkg  string
	name string
	decl *outputDecl
}

func (o *output) id() string { return "output." + o.pkg + "." + o.name }

// input is one input of a package in the stack: the values the imports pass
// to it, each evaluated in its import's scope as a part, joined by
// ztypes.Merge; its default, in the package's scope, when there is none.
type input struct {
	pkg  string
	name string
	decl *inputDecl
	args []*inputArg
}

func (in *input) id() string { return "input." + in.pkg + "." + in.name }

func (in *input) partID(i int) string { return fmt.Sprintf("%s.%d", in.id(), i) }

// part is one value an import passes to an input.
type part struct {
	in  *input
	i   int
	arg *inputArg
}

// gatherValues lists the inputs, with the values the imports pass to them,
// and the outputs of every package in the stack.
func (t *Tree) gatherValues() {
	for _, name := range sortedKeys(t.packages) {
		p := t.packages[name]
		if !t.active[pkgScope(name)] {
			continue
		}
		pb := p.file.block
		for _, n := range sortedKeys(pb.inputs) {
			t.inputs = append(t.inputs, &input{pkg: name, name: n, decl: pb.inputs[n], args: p.args[n]})
		}
		for _, o := range sortedKeys(pb.outputs) {
			t.outputs = append(t.outputs, &output{pkg: name, name: o, decl: pb.outputs[o]})
		}
	}
}

func (in *input) parts() []*part {
	out := make([]*part, len(in.args))
	for i, a := range in.args {
		out[i] = &part{in: in, i: i, arg: a}
	}
	return out
}

// defaultExpr is the expression evaluated when no import sets the input.
func (in *input) defaultExpr() hcl.Expression {
	if len(in.args) > 0 {
		return nil
	}
	return in.decl.def
}
