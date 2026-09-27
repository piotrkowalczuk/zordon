// Package ztypes is the type system of package inputs and outputs: which
// types a declaration may name, and how a value is checked against one.
//
// The supported types are string, number, bool, list(T), map(T) and
// object({ name = T, ... }), where an object attribute may be
// optional(T) or optional(T, default). They are written in HCL's type
// syntax, as in Terraform.
package ztypes

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// Type is a parsed input or output type. The zero value is not a type;
// Parse is the only way to get one.
type Type struct {
	ty       cty.Type
	defaults *typeexpr.Defaults
}

// Parse reads a type expression such as object({ host = string }).
func Parse(expr hcl.Expression) (Type, error) {
	ty, defaults, diags := typeexpr.TypeConstraintWithDefaults(expr)
	if diags.HasErrors() {
		return Type{}, fmt.Errorf("%s", strings.TrimSpace(diags.Error()))
	}
	if err := supported(ty, ""); err != nil {
		return Type{}, fmt.Errorf("%s: %w", expr.Range(), err)
	}
	return Type{ty: ty, defaults: defaults}, nil
}

// String renders the type as it is written, optional attributes included.
func (t Type) String() string {
	t.valid()
	return typeString(t.ty)
}

// IsObject reports whether the type is object({ ... }).
func (t Type) IsObject() bool {
	t.valid()
	return t.ty.IsObjectType()
}

// Attributes lists an object type's attributes, sorted; nil for any other
// type.
func (t Type) Attributes() []string {
	t.valid()
	if !t.ty.IsObjectType() {
		return nil
	}
	names := make([]string, 0, len(t.ty.AttributeTypes()))
	for n := range t.ty.AttributeTypes() {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// HasAttribute reports whether an object type declares name.
func (t Type) HasAttribute(name string) bool {
	t.valid()
	return t.ty.IsObjectType() && t.ty.HasAttribute(name)
}

// Optional reports whether an object type's attribute may be left out.
func (t Type) Optional(name string) bool {
	t.valid()
	return t.HasAttribute(name) && t.ty.AttributeOptional(name)
}

// Convert checks v against the type: it fills in optional attributes' defaults
// and converts what HCL converts implicitly, such as a number written as a
// string. An attribute the type does not declare is an error, where HCL would
// drop it silently. A mismatch names the path to the offending value.
func (t Type) Convert(v cty.Value) (cty.Value, error) {
	t.valid()
	if err := extra(v, t.ty, ""); err != nil {
		return cty.NilVal, err
	}
	if t.defaults != nil {
		v = t.defaults.Apply(v)
	}
	out, err := convert.Convert(v, t.ty)
	if err != nil {
		return cty.NilVal, describe(err)
	}
	return out, nil
}

// valid panics on the zero Type: using one is a programmer error, not bad
// input.
func (t Type) valid() {
	if t.ty == cty.NilType {
		panic("ztypes: zero Type; get one from Parse")
	}
}

// supported rejects the parts of HCL's type syntax an input or output may not
// use: any, whose values nothing can check; tuple and set, which a
// declaration does not need.
func supported(ty cty.Type, at string) error {
	where := func() string {
		if at == "" {
			return ""
		}
		return " at " + at
	}
	switch {
	case ty == cty.String, ty == cty.Number, ty == cty.Bool:
		return nil
	case ty == cty.DynamicPseudoType:
		return fmt.Errorf("type any%s is not supported: name the type, such as string or object({ ... })", where())
	case ty.IsListType():
		return supported(ty.ElementType(), at+"[*]")
	case ty.IsMapType():
		return supported(ty.ElementType(), at+"[*]")
	case ty.IsObjectType():
		for _, name := range sortedAttrs(ty) {
			if err := supported(ty.AttributeType(name), strings.TrimPrefix(at+"."+name, ".")); err != nil {
				return err
			}
		}
		return nil
	case ty.IsSetType():
		return fmt.Errorf("type set%s is not supported: use list", where())
	case ty.IsTupleType():
		return fmt.Errorf("type tuple%s is not supported: use list or object", where())
	}
	return fmt.Errorf("type %s%s is not supported", typeString(ty), where())
}

// extra finds an attribute of v that an object type in ty does not declare,
// looking through lists and maps.
func extra(v cty.Value, ty cty.Type, at string) error {
	if v.IsNull() || !v.IsKnown() {
		return nil
	}
	vt := v.Type()
	switch {
	case ty.IsObjectType() && (vt.IsObjectType() || vt.IsMapType()):
		for name, av := range v.AsValueMap() {
			path := strings.TrimPrefix(at+"."+name, ".")
			if !ty.HasAttribute(name) {
				return fmt.Errorf("%s: attribute not declared by %s", path, typeString(ty))
			}
			if err := extra(av, ty.AttributeType(name), path); err != nil {
				return err
			}
		}
	case ty.IsListType() && (vt.IsListType() || vt.IsTupleType() || vt.IsSetType()):
		for i, ev := range v.AsValueSlice() {
			if err := extra(ev, ty.ElementType(), fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
	case ty.IsMapType() && (vt.IsMapType() || vt.IsObjectType()):
		for k, ev := range v.AsValueMap() {
			if err := extra(ev, ty.ElementType(), fmt.Sprintf("%s[%q]", at, k)); err != nil {
				return err
			}
		}
	}
	return nil
}

func typeString(ty cty.Type) string {
	switch {
	case ty.IsListType():
		return "list(" + typeString(ty.ElementType()) + ")"
	case ty.IsMapType():
		return "map(" + typeString(ty.ElementType()) + ")"
	case ty.IsObjectType():
		attrs := sortedAttrs(ty)
		parts := make([]string, len(attrs))
		for i, name := range attrs {
			at := typeString(ty.AttributeType(name))
			if ty.AttributeOptional(name) {
				at = "optional(" + at + ")"
			}
			parts[i] = name + " = " + at
		}
		return "object({ " + strings.Join(parts, ", ") + " })"
	}
	return typeexpr.TypeString(ty)
}

func sortedAttrs(ty cty.Type) []string {
	names := make([]string, 0, len(ty.AttributeTypes()))
	for n := range ty.AttributeTypes() {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// describe turns a conversion error into one that names where in the value
// it went wrong.
func describe(err error) error {
	var pe cty.PathError
	if !errors.As(err, &pe) || len(pe.Path) == 0 {
		return err
	}
	var b strings.Builder
	for _, step := range pe.Path {
		switch s := step.(type) {
		case cty.GetAttrStep:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s.Name)
		case cty.IndexStep:
			switch {
			case s.Key.Type() == cty.String:
				fmt.Fprintf(&b, "[%q]", s.Key.AsString())
			case s.Key.Type() == cty.Number:
				fmt.Fprintf(&b, "[%s]", s.Key.AsBigFloat().Text('f', -1))
			default:
				b.WriteString("[…]")
			}
		}
	}
	return fmt.Errorf("%s: %s", b.String(), pe.Error())
}
