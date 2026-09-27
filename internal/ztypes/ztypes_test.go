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
		src, want, err string
	}{
		"string":                  {src: "string", want: "string"},
		"number":                  {src: "number", want: "number"},
		"bool":                    {src: "bool", want: "bool"},
		"list":                    {src: "list(string)", want: "list(string)"},
		"map":                     {src: "map(number)", want: "map(number)"},
		"object":                  {src: "object({ host = string, port = number })", want: "object({ host = string, port = number })"},
		"optional":                {src: "object({ host = string, tls = optional(bool) })", want: "object({ host = string, tls = optional(bool) })"},
		"optional with default":   {src: "object({ tls = optional(bool, false) })", want: "object({ tls = optional(bool) })"},
		"list of objects":         {src: "list(object({ path = string }))", want: "list(object({ path = string }))"},
		"map of lists":            {src: "map(list(string))", want: "map(list(string))"},
		"any":                     {src: "any", err: "type any is not supported"},
		"set":                     {src: "set(string)", err: "type set is not supported: use list"},
		"tuple":                   {src: "tuple([string, number])", err: "type tuple is not supported: use list or object"},
		"nested any":              {src: "object({ extra = any })", err: "type any at extra is not supported"},
		"any in list":             {src: "list(any)", err: "type any at [*] is not supported"},
		"set deep in an object":   {src: "object({ a = object({ b = set(string) }) })", err: "type set at a.b is not supported"},
		"unknown keyword":         {src: "strin", err: `The keyword "strin" is not a valid type specification`},
		"optional outside object": {src: "optional(string)", err: "optional"},
		"not a type":              {src: `"string"`, err: "A type specification is either a primitive type keyword"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			ty, err := Parse(expr(t, c.src))
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("want error %q, got %v", c.err, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := ty.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestType_attributes(t *testing.T) {
	ty := mustParse(t, "object({ port = number, host = string, tls = optional(bool, false) })")
	if !ty.IsObject() {
		t.Fatal("IsObject() = false")
	}
	if got := ty.Attributes(); strings.Join(got, ",") != "host,port,tls" {
		t.Errorf("Attributes() = %v, want sorted", got)
	}
	cases := map[string]struct{ has, optional bool }{
		"host": {has: true},
		"tls":  {has: true, optional: true},
		"nope": {},
	}
	for name, c := range cases {
		if got := ty.HasAttribute(name); got != c.has {
			t.Errorf("HasAttribute(%q) = %v, want %v", name, got, c.has)
		}
		if got := ty.Optional(name); got != c.optional {
			t.Errorf("Optional(%q) = %v, want %v", name, got, c.optional)
		}
	}
}

func TestType_attributes_notAnObject(t *testing.T) {
	ty := mustParse(t, "list(string)")
	if ty.IsObject() || ty.Attributes() != nil || ty.HasAttribute("x") || ty.Optional("x") {
		t.Errorf("a list type has no attributes: IsObject=%v Attributes=%v", ty.IsObject(), ty.Attributes())
	}
}

func TestType_Convert(t *testing.T) {
	cases := map[string]struct {
		typ, value, want, err string
	}{
		"string":                   {typ: "string", value: `"a"`, want: `"a"`},
		"number from string":       {typ: "number", value: `"8080"`, want: `8080`},
		"string from number":       {typ: "string", value: `8080`, want: `"8080"`},
		"bool from string":         {typ: "bool", value: `"true"`, want: `true`},
		"null":                     {typ: "string", value: `null`, want: `null`},
		"list":                     {typ: "list(number)", value: `[1, "2"]`, want: `[1, 2]`},
		"map":                      {typ: "map(string)", value: `{ a = 1 }`, want: `{ a = "1" }`},
		"object":                   {typ: "object({ host = string, port = number })", value: `{ host = "h", port = "80" }`, want: `{ host = "h", port = 80 }`},
		"optional default":         {typ: "object({ host = string, tls = optional(bool, false) })", value: `{ host = "h" }`, want: `{ host = "h", tls = false }`},
		"optional without default": {typ: "object({ host = string, tls = optional(bool) })", value: `{ host = "h" }`, want: `{ host = "h", tls = null }`},
		"default in a list":        {typ: "list(object({ path = optional(string, \"/\") }))", value: `[{}, { path = "/x" }]`, want: `[{ path = "/" }, { path = "/x" }]`},
		"default in a map":         {typ: "map(object({ n = optional(number, 1) }))", value: `{ a = {} }`, want: `{ a = { n = 1 } }`},
		"wrong primitive":          {typ: "number", value: `"eighty"`, err: "a number is required"},
		"missing attribute":        {typ: "object({ host = string, port = number })", value: `{ host = "h" }`, err: `attribute "port" is required`},
		"wrong attribute type":     {typ: "object({ port = number })", value: `{ port = "x" }`, err: "port: a number is required"},
		"wrong element in a list":  {typ: "list(object({ port = number }))", value: `[{ port = 1 }, { port = "x" }]`, err: "[1].port: a number is required"},
		"wrong value in a map":     {typ: "map(number)", value: `{ a = 1, b = "x" }`, err: `["b"]: a number is required`},
		"undeclared attribute":     {typ: "object({ host = string })", value: `{ host = "h", hots = "typo" }`, err: "hots: attribute not declared by object({ host = string })"},
		"undeclared nested":        {typ: "list(object({ a = object({ b = string }) }))", value: `[{ a = { b = "x", c = 1 } }]`, err: "[0].a.c: attribute not declared"},
		"undeclared in a map":      {typ: "map(object({ n = number }))", value: `{ k = { n = 1, m = 2 } }`, err: `["k"].m: attribute not declared`},
		"list for a string":        {typ: "string", value: `["a"]`, err: "string required"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := mustParse(t, c.typ).Convert(value(t, c.value))
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("want error %q, got %v (value %#v)", c.err, err, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := mustParse(t, c.typ)
			wantVal, err := want.Convert(value(t, c.want))
			if err != nil {
				t.Fatalf("want value: %v", err)
			}
			if !got.RawEquals(wantVal) {
				t.Errorf("Convert = %#v, want %#v", got, wantVal)
			}
		})
	}
}

func TestType_zeroPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("using the zero Type must panic")
		}
	}()
	_ = Type{}.String()
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
