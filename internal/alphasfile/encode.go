package alphasfile

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/piotrkowalczuk/zordon/internal/zdoc"
)

// encodeFuncs is the enc:: namespace: an HCL value serialized into a
// document. It exists so a generated file is built from structure rather
// than a hand-quoted heredoc, where an interpolated path or name carrying a
// quote or a backslash would corrupt the output silently.
func encodeFuncs() map[string]function.Function {
	return map[string]function.Function{
		"enc::json": encodeFunc(zdoc.FormatJSON),
		"enc::yaml": encodeFunc(zdoc.FormatYAML),
		"enc::toml": encodeFunc(zdoc.FormatTOML),
	}
}

func encodeFunc(f zdoc.Format) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{
			Name:             "value",
			Type:             cty.DynamicPseudoType,
			AllowNull:        true,
			AllowDynamicType: true,
		}},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			v, err := ctyToGo(args[0])
			if err != nil {
				return cty.NilVal, fmt.Errorf("enc::%s: %w", f, err)
			}
			b, err := zdoc.Encode(v, f)
			if err != nil {
				return cty.NilVal, fmt.Errorf("enc::%s: %w", f, err)
			}
			return cty.StringVal(string(b)), nil
		},
	})
}

// ctyToGo lowers a cty value into the plain Go tree zdoc consumes. Integral
// numbers stay integral: a port emitted as 8080.0 is rejected by most of what
// reads these files.
//
// Deliberately not ctyToAny: that one handles scalars only and falls back to
// v.GoString() for anything else, which would bake a cty debug string into a
// generated document. Here an unrepresentable value has to be an error.
func ctyToGo(v cty.Value) (any, error) {
	if v.IsNull() {
		return nil, nil
	}
	if !v.IsKnown() {
		return nil, errors.New("value is not known at evaluation time")
	}
	t := v.Type()
	switch {
	case t == cty.String:
		return v.AsString(), nil
	case t == cty.Bool:
		return v.True(), nil
	case t == cty.Number:
		f := v.AsBigFloat()
		// Int64's accuracy matters: an integer beyond int64 is still IsInt,
		// and Int64 clamps it. ctyToAny already checks this; silently writing
		// a clamped number into a generated document would be worse there.
		if i, acc := f.Int64(); f.IsInt() && acc == big.Exact {
			return i, nil
		}
		g, acc := f.Float64()
		if acc != big.Exact && f.IsInt() {
			return nil, fmt.Errorf("number %s does not fit any supported numeric type", f.Text('g', -1))
		}
		return g, nil
	case t.IsListType(), t.IsSetType(), t.IsTupleType():
		out := []any{}
		for it := v.ElementIterator(); it.Next(); {
			_, ev := it.Element()
			g, err := ctyToGo(ev)
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	case t.IsMapType(), t.IsObjectType():
		out := map[string]any{}
		for it := v.ElementIterator(); it.Next(); {
			k, ev := it.Element()
			g, err := ctyToGo(ev)
			if err != nil {
				return nil, err
			}
			out[k.AsString()] = g
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported type %s", t.FriendlyName())
}
