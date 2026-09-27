package ztypes

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

func TestParse(t *testing.T) {
	cases := map[string]struct {
		src  string
		want Type
	}{
		"bool":   {src: "bool", want: Bool{}},
		"string": {src: "string", want: String{}},
		"number": {src: "number", want: Number{}},
		"list":   {src: "list(string)", want: List{Elem: String{}}},
		"map":    {src: "map(number)", want: Map{Elem: Number{}}},
		"nested": {src: "map(list(bool))", want: Map{Elem: List{Elem: Bool{}}}},
		"object": {src: "object({ host = string, tls = optional(bool) })", want: Object{Attrs: map[string]Attribute{
			"host": {Type: String{}},
			"tls":  {Type: Bool{}, Optional: true},
		}}},
		"list of objects": {src: "list(object({ path = string }))", want: List{Elem: Object{Attrs: map[string]Attribute{"path": {Type: String{}}}}}},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := Parse(expr(t, c.src))
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != c.want.String() {
				t.Errorf("Parse(%q) = %s, want %s", c.src, got, c.want)
			}
		})
	}
}

func TestParse_optionalDefault(t *testing.T) {
	ty := mustParse(t, "object({ port = optional(number, 80) })")
	obj, ok := ty.(Object)
	if !ok {
		t.Fatalf("Parse = %T, want Object", ty)
	}
	a := obj.Attrs["port"]
	if !a.Optional || a.Default == nil || !a.Default.RawEquals(cty.NumberIntVal(80)) {
		t.Errorf("port = %+v, want optional with default 80", a)
	}
}

func TestParse_errors(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"any":               {src: "any", want: "type any is not supported"},
		"set":               {src: "set(string)", want: "type set is not supported: use list"},
		"tuple":             {src: "tuple([string])", want: "type tuple is not supported: use list or object"},
		"unknown keyword":   {src: "strin", want: `unknown type "strin"`},
		"int":               {src: "int", want: `unknown type "int"; use string, bool, number`},
		"unknown function":  {src: "vector(number)", want: "unknown type vector(...)"},
		"not a type":        {src: `"string"`, want: "a type is string, bool, number, list(T), map(T) or object"},
		"list without type": {src: "list()", want: "list takes one type"},
		"map with two":      {src: "map(string, number)", want: "map takes one type"},
		"object not a map":  {src: "object(string)", want: "object takes attributes"},
		"quoted attribute":  {src: `object({ "host" = string })`, want: "an object attribute is named by a bare word"},
		"repeated attr":     {src: "object({ a = string, a = number })", want: `attribute "a" is declared twice`},
		"optional alone":    {src: "optional(string)", want: "optional(...) marks an attribute of object"},
		"optional nested":   {src: "object({ a = optional(optional(string)) })", want: "optional(...) marks an attribute of object"},
		"optional too many": {src: "object({ a = optional(number, 1, 2) })", want: "optional takes a type and an optional default"},
		"bad default":       {src: `object({ a = optional(number, "eighty") })`, want: "the default does not fit number"},
		"default not const": {src: "object({ a = optional(string, var.x) })", want: "the default of optional must be a constant"},
		"any in a list":     {src: "list(any)", want: "type any is not supported"},
		"set in an object":  {src: "object({ a = set(string) })", want: "type set is not supported"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			_, err := Parse(expr(t, c.src))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestType_String(t *testing.T) {
	cases := map[string]Type{
		"bool":              Bool{},
		"string":            String{},
		"number":            Number{},
		"list(map(string))": List{Elem: Map{Elem: String{}}},
		"object({ a = number, b = optional(list(bool)) })": Object{Attrs: map[string]Attribute{
			"b": {Type: List{Elem: Bool{}}, Optional: true},
			"a": {Type: Number{}},
		}},
	}
	for want, ty := range cases {
		if got := ty.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func TestObject(t *testing.T) {
	obj := Object{Attrs: map[string]Attribute{
		"port": {Type: Number{}},
		"host": {Type: String{}},
		"tls":  {Type: Bool{}, Optional: true},
	}}
	if got := strings.Join(obj.Names(), ","); got != "host,port,tls" {
		t.Errorf("Names() = %s, want sorted", got)
	}
	if !obj.Has("host") || obj.Has("nope") {
		t.Errorf("Has: host=%v nope=%v", obj.Has("host"), obj.Has("nope"))
	}
	if obj.Optional("host") || !obj.Optional("tls") || obj.Optional("nope") {
		t.Errorf("Optional: host=%v tls=%v nope=%v", obj.Optional("host"), obj.Optional("tls"), obj.Optional("nope"))
	}
}

func TestConvert(t *testing.T) {
	cases := map[string]struct {
		typ, value string
		want       cty.Value
	}{
		"bool":                     {typ: "bool", value: `true`, want: cty.True},
		"bool from string":         {typ: "bool", value: `"false"`, want: cty.False},
		"string":                   {typ: "string", value: `"a"`, want: cty.StringVal("a")},
		"string from number":       {typ: "string", value: `8080`, want: cty.StringVal("8080")},
		"string from bool":         {typ: "string", value: `true`, want: cty.StringVal("true")},
		"number":                   {typ: "number", value: `8080`, want: cty.NumberIntVal(8080)},
		"fraction":                 {typ: "number", value: `0.5`, want: cty.NumberFloatVal(0.5)},
		"number from string":       {typ: "number", value: `" 8080 "`, want: cty.NumberIntVal(8080)},
		"null":                     {typ: "string", value: `null`, want: cty.NullVal(cty.String)},
		"list from tuple":          {typ: "list(number)", value: `[1, "2"]`, want: cty.ListVal([]cty.Value{cty.NumberIntVal(1), cty.NumberIntVal(2)})},
		"empty list":               {typ: "list(string)", value: `[]`, want: cty.ListValEmpty(cty.String)},
		"map from object":          {typ: "map(string)", value: `{ a = 1 }`, want: cty.MapVal(map[string]cty.Value{"a": cty.StringVal("1")})},
		"empty map":                {typ: "map(number)", value: `{}`, want: cty.MapValEmpty(cty.Number)},
		"object":                   {typ: "object({ host = string, port = number })", value: `{ host = "h", port = "80" }`, want: cty.ObjectVal(map[string]cty.Value{"host": cty.StringVal("h"), "port": cty.NumberIntVal(80)})},
		"optional default":         {typ: "object({ tls = optional(bool, false) })", value: `{}`, want: cty.ObjectVal(map[string]cty.Value{"tls": cty.False})},
		"optional without default": {typ: "object({ tls = optional(bool) })", value: `{}`, want: cty.ObjectVal(map[string]cty.Value{"tls": cty.NullVal(cty.Bool)})},
		"default in a list":        {typ: `list(object({ path = optional(string, "/") }))`, value: `[{}]`, want: cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"path": cty.StringVal("/")})})},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := Convert(mustParse(t, c.typ), value(t, c.value))
			if err != nil {
				t.Fatal(err)
			}
			if !got.RawEquals(c.want) {
				t.Errorf("Convert = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestConvert_unknownValue(t *testing.T) {
	got, err := Convert(List{Elem: String{}}, cty.UnknownVal(cty.DynamicPseudoType))
	if err != nil || got.IsKnown() || !got.Type().Equals(cty.List(cty.String)) {
		t.Errorf("Convert of an unknown = %#v, %v; want an unknown list(string)", got, err)
	}
}

func TestConvert_errors(t *testing.T) {
	cases := map[string]struct{ typ, value, want string }{
		"bool from text":          {typ: "bool", value: `"yes"`, want: "a bool is required, got string"},
		"string from list":        {typ: "string", value: `["a"]`, want: "a string is required, got tuple"},
		"number from text":        {typ: "number", value: `"eighty"`, want: "a number is required, got string"},
		"number from bool":        {typ: "number", value: `true`, want: "a number is required, got bool"},
		"list from string":        {typ: "list(string)", value: `"a"`, want: "a list is required"},
		"map from list":           {typ: "map(string)", value: `["a"]`, want: "a map is required"},
		"object from string":      {typ: "object({ a = string })", value: `"a"`, want: "an object is required"},
		"missing attribute":       {typ: "object({ host = string, port = number })", value: `{ host = "h" }`, want: `attribute "port" is required`},
		"undeclared attribute":    {typ: "object({ host = string })", value: `{ host = "h", hots = "typo" }`, want: "hots: attribute not declared by object({ host = string })"},
		"wrong attribute":         {typ: "object({ port = number })", value: `{ port = "x" }`, want: "port: a number is required"},
		"wrong element in a list": {typ: "list(object({ port = number }))", value: `[{ port = 1 }, { port = "x" }]`, want: "[1].port: a number is required"},
		"wrong value in a map":    {typ: "map(number)", value: `{ a = 1, b = "x" }`, want: `["b"]: a number is required`},
		"undeclared nested":       {typ: "list(object({ a = object({ b = string }) }))", value: `[{ a = { b = "x", c = 1 } }]`, want: "[0].a.c: attribute not declared"},
		"missing nested":          {typ: "map(object({ n = number }))", value: `{ k = {} }`, want: `["k"]: attribute "n" is required`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			_, err := Convert(mustParse(t, c.typ), value(t, c.value))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestConvert_nilTypePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Convert with a nil Type must panic")
		}
	}()
	_, _ = Convert(nil, cty.True)
}

func mustParse(t *testing.T, src string) Type {
	t.Helper()
	ty, err := Parse(expr(t, src))
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	return ty
}

func expr(t *testing.T, src string) hcl.Expression {
	t.Helper()
	e, diags := hclsyntax.ParseExpression([]byte(src), "t.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parse %q: %s", src, diags.Error())
	}
	return e
}

func value(t *testing.T, src string) cty.Value {
	t.Helper()
	v, diags := expr(t, src).Value(nil)
	if diags.HasErrors() {
		t.Fatalf("value %q: %s", src, diags.Error())
	}
	return v
}
