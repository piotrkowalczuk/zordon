// Package ztypes is the type system of package inputs and outputs: a closed
// union of the types a declaration may name, parsed from HCL and used to
// check a value.
//
// In HCL a type is written as string, bool, number, list(T), map(T) or
// object({ name = T, ... }), where an object attribute may be optional(T) or
// optional(T, default).
package ztypes

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Type is one of Bool, String, Number, List, Map and Object.
type Type interface {
	// String renders the type as it is written.
	String() string
	// cty is the type of the values Convert returns.
	cty() cty.Type
	convert(v cty.Value, at string) (cty.Value, error)
}

// Bool is true or false.
type Bool struct{}

// String is text.
type String struct{}

// Number is a number.
type Number struct{}

// List is an ordered sequence of Elem.
type List struct{ Elem Type }

// Map is string keys to Elem.
type Map struct{ Elem Type }

// Object is named attributes.
type Object struct{ Attrs map[string]Attribute }

// Attribute is one attribute of an Object. An optional attribute may be left
// out; it is then its Default, or null when Default is nil.
type Attribute struct {
	Type     Type
	Optional bool
	Default  *cty.Value
}

func (Bool) String() string   { return "bool" }
func (String) String() string { return "string" }
func (Number) String() string { return "number" }
func (t List) String() string { return "list(" + t.Elem.String() + ")" }
func (t Map) String() string  { return "map(" + t.Elem.String() + ")" }

func (t Object) String() string {
	names := t.Names()
	parts := make([]string, len(names))
	for i, n := range names {
		a := t.Attrs[n]
		s := a.Type.String()
		if a.Optional {
			s = "optional(" + s + ")"
		}
		parts[i] = n + " = " + s
	}
	return "object({ " + strings.Join(parts, ", ") + " })"
}

// Names lists the object's attributes, sorted.
func (t Object) Names() []string {
	names := make([]string, 0, len(t.Attrs))
	for n := range t.Attrs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Has reports whether the object declares name.
func (t Object) Has(name string) bool {
	_, ok := t.Attrs[name]
	return ok
}

// Optional reports whether name may be left out.
func (t Object) Optional(name string) bool { return t.Attrs[name].Optional }

func (Bool) cty() cty.Type   { return cty.Bool }
func (String) cty() cty.Type { return cty.String }
func (Number) cty() cty.Type { return cty.Number }
func (t List) cty() cty.Type { return cty.List(t.Elem.cty()) }
func (t Map) cty() cty.Type  { return cty.Map(t.Elem.cty()) }

func (t Object) cty() cty.Type {
	attrs := make(map[string]cty.Type, len(t.Attrs))
	for n, a := range t.Attrs {
		attrs[n] = a.Type.cty()
	}
	return cty.Object(attrs)
}

// Convert checks v against t and returns it as t's values are: a number
// written as a string becomes a number, an optional attribute left out takes
// its default. A value that does not fit is an error naming where in the
// value it went wrong.
func Convert(t Type, v cty.Value) (cty.Value, error) {
	if t == nil {
		panic("ztypes: Convert with a nil Type")
	}
	return t.convert(v, "")
}

// Parse reads a type expression such as object({ host = string, port = number }).
func Parse(expr hcl.Expression) (Type, error) {
	return parse(expr)
}

func parse(expr hcl.Expression) (Type, error) {
	at := expr.Range()
	if kw := hcl.ExprAsKeyword(expr); kw != "" {
		switch kw {
		case "bool":
			return Bool{}, nil
		case "string":
			return String{}, nil
		case "number":
			return Number{}, nil
		case "any":
			return nil, fmt.Errorf("%s: type any is not supported: name the type, such as string or object({ ... })", at)
		}
		return nil, fmt.Errorf("%s: unknown type %q; use string, bool, number, list(T), map(T) or object({ ... })", at, kw)
	}
	call, ok := expr.(*hclsyntax.FunctionCallExpr)
	if !ok {
		return nil, fmt.Errorf("%s: a type is string, bool, number, list(T), map(T) or object({ ... })", at)
	}
	one := func() (hcl.Expression, error) {
		if len(call.Args) != 1 {
			return nil, fmt.Errorf("%s: %s takes one type, such as %s(string)", at, call.Name, call.Name)
		}
		return call.Args[0], nil
	}
	switch call.Name {
	case "list", "map":
		arg, err := one()
		if err != nil {
			return nil, err
		}
		elem, err := parse(arg)
		if err != nil {
			return nil, err
		}
		if call.Name == "list" {
			return List{Elem: elem}, nil
		}
		return Map{Elem: elem}, nil
	case "object":
		arg, err := one()
		if err != nil {
			return nil, err
		}
		return parseObject(arg)
	case "optional":
		return nil, fmt.Errorf("%s: optional(...) marks an attribute of object({ ... }) and cannot stand alone", at)
	case "set":
		return nil, fmt.Errorf("%s: type set is not supported: use list", at)
	case "tuple":
		return nil, fmt.Errorf("%s: type tuple is not supported: use list or object", at)
	}
	return nil, fmt.Errorf("%s: unknown type %s(...); use list(T), map(T) or object({ ... })", at, call.Name)
}

func parseObject(expr hcl.Expression) (Type, error) {
	cons, ok := expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return nil, fmt.Errorf("%s: object takes attributes, such as object({ host = string })", expr.Range())
	}
	out := Object{Attrs: map[string]Attribute{}}
	for _, item := range cons.Items {
		name := hcl.ExprAsKeyword(item.KeyExpr)
		if name == "" {
			return nil, fmt.Errorf("%s: an object attribute is named by a bare word, such as host = string", item.KeyExpr.Range())
		}
		if _, dup := out.Attrs[name]; dup {
			return nil, fmt.Errorf("%s: attribute %q is declared twice", item.KeyExpr.Range(), name)
		}
		attr, err := parseAttribute(item.ValueExpr)
		if err != nil {
			return nil, err
		}
		out.Attrs[name] = attr
	}
	return out, nil
}

func parseAttribute(expr hcl.Expression) (Attribute, error) {
	call, ok := expr.(*hclsyntax.FunctionCallExpr)
	if !ok || call.Name != "optional" {
		t, err := parse(expr)
		return Attribute{Type: t}, err
	}
	if len(call.Args) < 1 || len(call.Args) > 2 {
		return Attribute{}, fmt.Errorf("%s: optional takes a type and an optional default, such as optional(number, 80)", expr.Range())
	}
	t, err := parse(call.Args[0])
	if err != nil {
		return Attribute{}, err
	}
	attr := Attribute{Type: t, Optional: true}
	if len(call.Args) == 2 {
		v, diags := call.Args[1].Value(nil)
		if diags.HasErrors() {
			return Attribute{}, fmt.Errorf("%s: the default of optional must be a constant: %s", call.Args[1].Range(), strings.TrimSpace(diags.Error()))
		}
		d, err := t.convert(v, "")
		if err != nil {
			return Attribute{}, fmt.Errorf("%s: the default does not fit %s: %w", call.Args[1].Range(), t, err)
		}
		attr.Default = &d
	}
	return attr, nil
}

func (t Bool) convert(v cty.Value, at string) (cty.Value, error) {
	if v, done := passThrough(t, v); done {
		return v, nil
	}
	switch v.Type() {
	case cty.Bool:
		return v, nil
	case cty.String:
		switch v.AsString() {
		case "true":
			return cty.True, nil
		case "false":
			return cty.False, nil
		}
	}
	return cty.NilVal, mismatch(at, "a bool is required", v)
}

func (t String) convert(v cty.Value, at string) (cty.Value, error) {
	if v, done := passThrough(t, v); done {
		return v, nil
	}
	switch v.Type() {
	case cty.String:
		return v, nil
	case cty.Bool:
		if v.True() {
			return cty.StringVal("true"), nil
		}
		return cty.StringVal("false"), nil
	case cty.Number:
		return cty.StringVal(v.AsBigFloat().Text('f', -1)), nil
	}
	return cty.NilVal, mismatch(at, "a string is required", v)
}

func (t Number) convert(v cty.Value, at string) (cty.Value, error) {
	if v, done := passThrough(t, v); done {
		return v, nil
	}
	switch v.Type() {
	case cty.Number:
		return v, nil
	case cty.String:
		if f, ok := new(big.Float).SetString(strings.TrimSpace(v.AsString())); ok {
			return cty.NumberVal(f), nil
		}
	}
	return cty.NilVal, mismatch(at, "a number is required", v)
}

func (t List) convert(v cty.Value, at string) (cty.Value, error) {
	if v, done := passThrough(t, v); done {
		return v, nil
	}
	vt := v.Type()
	if !vt.IsListType() && !vt.IsTupleType() && !vt.IsSetType() {
		return cty.NilVal, mismatch(at, "a list is required", v)
	}
	elems := v.AsValueSlice()
	if len(elems) == 0 {
		return cty.ListValEmpty(t.Elem.cty()), nil
	}
	out := make([]cty.Value, len(elems))
	for i, e := range elems {
		c, err := t.Elem.convert(e, fmt.Sprintf("%s[%d]", at, i))
		if err != nil {
			return cty.NilVal, err
		}
		out[i] = c
	}
	return cty.ListVal(out), nil
}

func (t Map) convert(v cty.Value, at string) (cty.Value, error) {
	if v, done := passThrough(t, v); done {
		return v, nil
	}
	vt := v.Type()
	if !vt.IsMapType() && !vt.IsObjectType() {
		return cty.NilVal, mismatch(at, "a map is required", v)
	}
	elems := v.AsValueMap()
	if len(elems) == 0 {
		return cty.MapValEmpty(t.Elem.cty()), nil
	}
	out := make(map[string]cty.Value, len(elems))
	for k, e := range elems {
		c, err := t.Elem.convert(e, fmt.Sprintf("%s[%q]", at, k))
		if err != nil {
			return cty.NilVal, err
		}
		out[k] = c
	}
	return cty.MapVal(out), nil
}

func (t Object) convert(v cty.Value, at string) (cty.Value, error) {
	if v, done := passThrough(t, v); done {
		return v, nil
	}
	vt := v.Type()
	if !vt.IsObjectType() && !vt.IsMapType() {
		return cty.NilVal, mismatch(at, "an object is required", v)
	}
	given := v.AsValueMap()
	for _, name := range sortedKeys(given) {
		if !t.Has(name) {
			return cty.NilVal, fmt.Errorf("%s: attribute not declared by %s", join(at, name), t)
		}
	}
	out := make(map[string]cty.Value, len(t.Attrs))
	for _, name := range t.Names() {
		a := t.Attrs[name]
		av, ok := given[name]
		switch {
		case ok:
			c, err := a.Type.convert(av, join(at, name))
			if err != nil {
				return cty.NilVal, err
			}
			out[name] = c
		case a.Default != nil:
			out[name] = *a.Default
		case a.Optional:
			out[name] = cty.NullVal(a.Type.cty())
		default:
			return cty.NilVal, fmt.Errorf("%sattribute %q is required", prefix(at), name)
		}
	}
	return cty.ObjectVal(out), nil
}

// passThrough handles what every type accepts as it is: null and a value not
// known yet.
func passThrough(t Type, v cty.Value) (cty.Value, bool) {
	switch {
	case v.IsNull():
		return cty.NullVal(t.cty()), true
	case !v.IsKnown():
		return cty.UnknownVal(t.cty()), true
	}
	return v, false
}

func mismatch(at, want string, v cty.Value) error {
	return fmt.Errorf("%s%s, got %s", prefix(at), want, v.Type().FriendlyName())
}

func prefix(at string) string {
	if at == "" {
		return ""
	}
	return at + ": "
}

func join(at, name string) string {
	if at == "" {
		return name
	}
	return at + "." + name
}

func sortedKeys(m map[string]cty.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
