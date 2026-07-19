package logfilter

import (
	"fmt"
	"strconv"
	"strings"
)

// The filter DSL is a small boolean language over a single log line. It is
// deliberately not a general expression language: the whole grammar is
// predicates that a line either satisfies or not, so a compiled filter is a
// pure, total, side-effect-free predicate tree (which is what lets the
// planner reorder it freely — see plan.go).
//
// Grammar (precedence low→high: or, and, not, comparison, primary):
//
//	expr    = or
//	or      = and ("or" and)*
//	and     = not ("and" not)*
//	not     = "not" not | primary
//	primary = "(" expr ")" | call | comparison | bool
//	call    = ident "(" arg ("," arg)* ")"
//	comp    = value cmpOp value
//	value   = call | ident | string | number
//	cmpOp   = "==" | "!=" | "<" | "<=" | ">" | ">="

type node interface{ isNode() }

type (
	binNode struct {
		op   string
		l, r node
	} // and | or | == | != | < | <= | > | >=
	notNode  struct{ x node }
	callNode struct {
		name string
		args []node
	}
	identNode  struct{ name string }
	strNode    struct{ v string }
	intNode    struct{ v int64 }
	floatNode  struct{ v float64 }
	boolLitNod struct{ v bool }
)

func (binNode) isNode()    {}
func (notNode) isNode()    {}
func (callNode) isNode()   {}
func (identNode) isNode()  {}
func (strNode) isNode()    {}
func (intNode) isNode()    {}
func (floatNode) isNode()  {}
func (boolLitNod) isNode() {}

// parse turns DSL source into an AST. It reports the first syntax error with
// the offending position; callers wrap it with the filter context.
func parse(src string) (node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, fmt.Errorf("unexpected %q at column %d", p.peek().text, p.peek().col)
	}
	return n, nil
}

// --- lexer -----------------------------------------------------------------

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tString
	tInt
	tFloat
	tOp      // == != < <= > >= ( ) ,
	tKeyword // and or not
)

type token struct {
	kind tokKind
	text string
	col  int
}

func lex(src string) ([]token, error) {
	var out []token
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')' || c == ',':
			out = append(out, token{tOp, string(c), i + 1})
			i++
		case c == '=' || c == '!' || c == '<' || c == '>':
			switch {
			case i+1 < n && src[i+1] == '=':
				out = append(out, token{tOp, src[i : i+2], i + 1})
				i += 2
			case c == '<' || c == '>':
				out = append(out, token{tOp, string(c), i + 1})
				i++
			default:
				return nil, fmt.Errorf("unexpected %q at column %d (did you mean == or !=?)", string(c), i+1)
			}
		case c == '"':
			s, adv, err := lexString(src[i:], i+1)
			if err != nil {
				return nil, err
			}
			out = append(out, token{tString, s, i + 1})
			i += adv
		case c >= '0' && c <= '9' || c == '-' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9':
			tok, adv := lexNumber(src[i:], i+1)
			out = append(out, tok)
			i += adv
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentPart(src[j]) {
				j++
			}
			word := src[i:j]
			k := tIdent
			if word == "and" || word == "or" || word == "not" {
				k = tKeyword
			}
			out = append(out, token{k, word, i + 1})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q at column %d", string(c), i+1)
		}
	}
	out = append(out, token{tEOF, "", n + 1})
	return out, nil
}

func lexString(s string, col int) (string, int, error) {
	var b strings.Builder
	i := 1 // skip opening quote
	for i < len(s) {
		c := s[i]
		if c == '"' {
			return b.String(), i + 1, nil
		}
		if c == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(s[i])
			}
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	return "", 0, fmt.Errorf("unterminated string at column %d", col)
}

func lexNumber(s string, col int) (token, int) {
	i := 0
	if s[0] == '-' {
		i++
	}
	dot := false
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		if s[i] == '.' {
			dot = true
		}
		i++
	}
	if dot {
		return token{tFloat, s[:i], col}, i
	}
	return token{tInt, s[:i], col}, i
}

func isIdentStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9'
}

// --- parser ----------------------------------------------------------------

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) parseExpr() (node, error) { return p.parseOr() }

func (p *parser) parseOr() (node, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tKeyword && p.peek().text == "or" {
		p.next()
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &binNode{op: "or", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseAnd() (node, error) {
	l, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tKeyword && p.peek().text == "and" {
		p.next()
		r, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l = &binNode{op: "and", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseNot() (node, error) {
	if p.peek().kind == tKeyword && p.peek().text == "not" {
		p.next()
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &notNode{x: x}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (node, error) {
	t := p.peek()
	if t.kind == tOp && t.text == "(" {
		p.next()
		n, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().text != ")" {
			return nil, fmt.Errorf("expected ) at column %d", p.peek().col)
		}
		p.next()
		return n, nil
	}
	// A primary is a value; if a comparison operator follows it becomes a
	// comparison, otherwise it must stand alone as a boolean (a bool-valued
	// call or literal) — validated later in plan.go.
	left, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if op := p.peek(); op.kind == tOp && isCmpOp(op.text) {
		p.next()
		right, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return &binNode{op: op.text, l: left, r: right}, nil
	}
	return left, nil
}

func (p *parser) parseValue() (node, error) {
	t := p.next()
	switch t.kind {
	case tString:
		return &strNode{v: t.text}, nil
	case tInt:
		v, _ := strconv.ParseInt(t.text, 10, 64)
		return &intNode{v: v}, nil
	case tFloat:
		v, _ := strconv.ParseFloat(t.text, 64)
		return &floatNode{v: v}, nil
	case tIdent:
		if t.text == "true" || t.text == "false" {
			return &boolLitNod{v: t.text == "true"}, nil
		}
		if p.peek().kind == tOp && p.peek().text == "(" {
			return p.parseCall(t.text)
		}
		return &identNode{name: t.text}, nil
	default:
		return nil, fmt.Errorf("unexpected %q at column %d", t.text, t.col)
	}
}

func (p *parser) parseCall(name string) (node, error) {
	p.next() // consume (
	call := &callNode{name: name}
	if p.peek().text == ")" {
		p.next()
		return call, nil
	}
	for {
		arg, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		call.args = append(call.args, arg)
		switch p.peek().text {
		case ",":
			p.next()
		case ")":
			p.next()
			return call, nil
		default:
			return nil, fmt.Errorf("expected , or ) at column %d", p.peek().col)
		}
	}
}

func isCmpOp(s string) bool {
	switch s {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}
