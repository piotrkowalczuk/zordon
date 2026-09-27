package ztypes

import (
	"fmt"
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

func TestMerge(t *testing.T) {
	sites := "map(object({ host = string, port = optional(number, 80) }))"
	cases := map[string]struct {
		typ   string
		parts []string
		want  cty.Value
	}{
		"no parts":         {typ: "string", want: cty.NullVal(cty.String)},
		"one string":       {typ: "string", parts: []string{`"a"`}, want: cty.StringVal("a")},
		"converted":        {typ: "number", parts: []string{`"8080"`}, want: cty.NumberIntVal(8080)},
		"null and one":     {typ: "bool", parts: []string{`null`, `true`}, want: cty.True},
		"only nulls":       {typ: "list(string)", parts: []string{`null`, `null`}, want: cty.NullVal(cty.List(cty.String))},
		"one object":       {typ: "object({ a = string })", parts: []string{`{ a = "x" }`}, want: cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("x")})},
		"one map":          {typ: "map(string)", parts: []string{`{ a = "x" }`}, want: cty.MapVal(map[string]cty.Value{"a": cty.StringVal("x")})},
		"maps joined":      {typ: "map(string)", parts: []string{`{ a = "x" }`, `{ b = "y", c = "z" }`}, want: cty.MapVal(map[string]cty.Value{"a": cty.StringVal("x"), "b": cty.StringVal("y"), "c": cty.StringVal("z")})},
		"empty map":        {typ: "map(number)", parts: []string{`{}`}, want: cty.MapValEmpty(cty.Number)},
		"empty and entry":  {typ: "map(number)", parts: []string{`{}`, `{ a = 1 }`}, want: cty.MapVal(map[string]cty.Value{"a": cty.NumberIntVal(1)})},
		"null map skipped": {typ: "map(number)", parts: []string{`null`, `{ a = 1 }`}, want: cty.MapVal(map[string]cty.Value{"a": cty.NumberIntVal(1)})},
		"entries converted": {typ: sites, parts: []string{`{ shop = { host = "shop.test" } }`, `{ blog = { host = "blog.test", port = "81" } }`}, want: cty.MapVal(map[string]cty.Value{
			"shop": cty.ObjectVal(map[string]cty.Value{"host": cty.StringVal("shop.test"), "port": cty.NumberIntVal(80)}),
			"blog": cty.ObjectVal(map[string]cty.Value{"host": cty.StringVal("blog.test"), "port": cty.NumberIntVal(81)}),
		})},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := Merge(mustParse(t, c.typ), parts(t, c.parts...))
			if err != nil {
				t.Fatal(err)
			}
			if !got.RawEquals(c.want) {
				t.Errorf("Merge = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestMerge_unique(t *testing.T) {
	ty := mustParse(t, "map(object({ host = string, port = optional(number) }))")
	got, err := Merge(ty, parts(t, `{ shop = { host = "shop.test", port = 80 } }`, `{ blog = { host = "blog.test" }, api = { host = "api.test" } }`), "host", "port")
	if err != nil {
		t.Fatal(err)
	}
	if n := got.LengthInt(); n != 3 {
		t.Errorf("Merge kept %d entries, want 3", n)
	}
}

func TestMerge_unknown(t *testing.T) {
	ty := Map{Elem: String{}}
	got, err := Merge(ty, []Part{{Value: cty.MapVal(map[string]cty.Value{"a": cty.StringVal("x")}), At: "p0"}, {Value: cty.UnknownVal(cty.Map(cty.String)), At: "p1"}})
	if err != nil || got.IsKnown() || !got.Type().Equals(cty.Map(cty.String)) {
		t.Errorf("Merge with an unknown part = %#v, %v; want an unknown map(string)", got, err)
	}
}

func TestMerge_errors(t *testing.T) {
	sites := "map(object({ host = string, port = optional(number) }))"
	cases := map[string]struct {
		typ    string
		parts  []string
		unique []string
		want   string
	}{
		"string twice":     {typ: "string", parts: []string{`"a"`, `"b"`}, want: "p1: already set at p0; a string takes one value"},
		"same value twice": {typ: "string", parts: []string{`"a"`, `"a"`}, want: "p1: already set at p0"},
		"number twice":     {typ: "number", parts: []string{`1`, `null`, `2`}, want: "p2: already set at p0; a number takes one value"},
		"list twice":       {typ: "list(string)", parts: []string{`["a"]`, `["b"]`}, want: "p1: already set at p0; a list(string) takes one value"},
		"object twice":     {typ: "object({ a = string })", parts: []string{`{ a = "x" }`, `{ a = "y" }`}, want: "p1: already set at p0; a object({ a = string }) takes one value"},
		"single mismatch":  {typ: "number", parts: []string{`"eighty"`}, want: "p0: a number is required, got string"},
		"key twice":        {typ: "map(string)", parts: []string{`{ a = "x", b = "y" }`, `{ b = "z" }`}, want: `p1: key "b" is already set at p0`},
		"key twice at one": {typ: "map(string)", parts: []string{`{ a = "x" }`, `{ c = "y" }`, `{ a = "z" }`}, want: `p2: key "a" is already set at p0`},
		"entry mismatch":   {typ: sites, parts: []string{`{ shop = { host = "h" } }`, `{ blog = { port = 1 } }`}, want: `p1: ["blog"]: attribute "host" is required`},
		"map from string":  {typ: "map(string)", parts: []string{`"a"`}, want: "p0: a map is required, got string"},
		"unique across":    {typ: sites, parts: []string{`{ shop = { host = "a.test" } }`, `{ blog = { host = "a.test" } }`}, unique: []string{"host"}, want: `p1: entry "blog": host = "a.test" is already used by entry "shop" at p0`},
		"unique within":    {typ: sites, parts: []string{`{ x = { host = "a.test" }, y = { host = "a.test" } }`}, unique: []string{"host"}, want: `p0: entry "y": host = "a.test" is already used by entry "x" at p0`},
		"unique bool":      {typ: "map(object({ on = bool }))", parts: []string{`{ a = { on = false }, b = { on = "false" } }`}, unique: []string{"on"}, want: `p0: entry "b": on = false is already used by entry "a" at p0`},
		"unique list":      {typ: "map(object({ tags = list(string) }))", parts: []string{`{ a = { tags = ["x"] } }`, `{ b = { tags = ["x"] } }`}, unique: []string{"tags"}, want: `entry "b": tags = ["x"] is already used`},
		"unique object":    {typ: "map(object({ at = object({ h = string, p = number }) }))", parts: []string{`{ a = { at = { h = "x", p = 1 } }, b = { at = { p = 1, h = "x" } } }`}, unique: []string{"at"}, want: `entry "b": at = { h = "x", p = 1 } is already used`},
		"unique converted": {typ: sites, parts: []string{`{ a = { host = "h1", port = 80 } }`, `{ b = { host = "h2", port = "80" } }`}, unique: []string{"host", "port"}, want: `p1: entry "b": port = 80 is already used by entry "a" at p0`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			_, err := Merge(mustParse(t, c.typ), parts(t, c.parts...), c.unique...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestMerge_uniqueSkipsNull(t *testing.T) {
	ty := mustParse(t, "map(object({ host = optional(string) }))")
	if _, err := Merge(ty, parts(t, `{ a = {} }`, `{ b = {} }`), "host"); err != nil {
		t.Errorf("two entries leaving a unique attribute out: %v", err)
	}
}

func TestMerge_panics(t *testing.T) {
	cases := map[string]struct {
		typ    Type
		unique []string
	}{
		"nil type":              {typ: nil},
		"unique on a string":    {typ: String{}, unique: []string{"host"}},
		"unique on map(string)": {typ: Map{Elem: String{}}, unique: []string{"host"}},
		"unique undeclared":     {typ: Map{Elem: Object{Attrs: map[string]Attribute{"host": {Type: String{}}}}}, unique: []string{"port"}},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Merge must panic")
				}
			}()
			_, _ = Merge(c.typ, nil, c.unique...)
		})
	}
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

func parts(t *testing.T, srcs ...string) []Part {
	t.Helper()
	out := make([]Part, len(srcs))
	for i, src := range srcs {
		out[i] = Part{Value: value(t, src), At: fmt.Sprintf("p%d", i)}
	}
	return out
}
