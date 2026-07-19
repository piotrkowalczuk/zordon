// Package logfilter compiles a small boolean DSL into a fast, allocation-lean
// predicate over a single raw log line. It backs the per-service
// `log { filter = "…" }` block: a line for which the predicate is true is
// dropped (never written to the log file nor forwarded to the client).
//
// Design goals, in order: (1) drop cheaply at extreme volume — the common
// case (a substring/prefix/regex on the raw bytes) never allocates; (2) allow
// structured matches without parsing the whole line — json()/logfmt()/
// severity() extract only the referenced field, lazily, via short-circuit;
// (3) stay fast regardless of how the user orders the expression — the
// compiler reorders each and/or by measured cost so the cheapest predicate
// gates the rest (see plan.go's cost constants).
//
// Grammar and node shapes live in parse.go; severity/field scanning in
// severity.go.
package logfilter

import (
	"bytes"
	"regexp"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// Filter is a compiled, reusable, concurrency-safe predicate. The zero value
// and a nil *Filter both mean "keep everything".
type Filter struct {
	root predicate
}

// Compile parses and compiles a filter expression. An empty (or whitespace)
// expression compiles to a nil *Filter, which keeps every line. A syntax or
// type error is returned as error (it is user/config input, never a panic).
func Compile(src string) (*Filter, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	n, err := parse(src)
	if err != nil {
		return nil, err
	}
	root, err := compileBool(n)
	if err != nil {
		return nil, err
	}
	return &Filter{root: root}, nil
}

// Validate reports whether src is a well-formed filter, without keeping the
// compiled result. Config resolution calls it so errors surface before start.
func Validate(src string) error {
	_, err := Compile(src)
	return err
}

// Drop reports whether the line should be suppressed. line is the raw bytes
// (as scanned, no trailing newline); stream is "stdout"/"stderr"; service is
// the emitting service's name. A nil *Filter never drops.
func (f *Filter) Drop(line []byte, stream, service string) bool {
	if f == nil || f.root == nil {
		return false
	}
	return f.root.match(line, stream, service)
}

// --- evaluation ------------------------------------------------------------

// match takes the line context by value rather than a shared struct pointer:
// an interface call forces a pointer argument to escape, which would heap-
// allocate one context per line and dominate GC on a talkative service. The
// header copies here stay on the stack, keeping the fast path allocation-free.
type predicate interface {
	match(raw []byte, stream, service string) bool
	cost() int
}

// cost weights are relative, anchored to the benchmark (per-call, normalized
// to a substring test): metadata is free, a substring is the unit, a lazy
// field extraction is ~60–75×, and a regex is the most expensive at ~400×.
const (
	costMeta     = 0
	costLiteral  = 1
	costLogfmt   = 60
	costSeverity = 60
	costJSON     = 75
	costRegex    = 400
)

type substrPred struct {
	mode byte // 'c' contains, 'p' prefix, 's' suffix
	sub  []byte
}

func (p substrPred) match(raw []byte, _, _ string) bool {
	switch p.mode {
	case 'p':
		return bytes.HasPrefix(raw, p.sub)
	case 's':
		return bytes.HasSuffix(raw, p.sub)
	default:
		return bytes.Contains(raw, p.sub)
	}
}
func (p substrPred) cost() int { return costLiteral }

type regexPred struct {
	re     *regexp.Regexp
	prefix []byte // necessary literal prefix for `^literal…` patterns, or nil
}

func (p *regexPred) match(raw []byte, _, _ string) bool {
	if p.prefix != nil && !bytes.HasPrefix(raw, p.prefix) {
		return false
	}
	return p.re.Match(raw)
}
func (p *regexPred) cost() int { return costRegex }

type metaPred struct {
	field byte // 's' stream, 'v' service
	want  string
	ne    bool
}

func (p metaPred) match(_ []byte, stream, service string) bool {
	got := stream
	if p.field == 'v' {
		got = service
	}
	if p.ne {
		return got != p.want
	}
	return got == p.want
}
func (p metaPred) cost() int { return costMeta }

type severityPred struct {
	op        string
	threshold int
}

func (p severityPred) match(raw []byte, _, _ string) bool {
	s := scanSeverity(raw)
	if s == severityUnknown {
		return false // unclassifiable line: keep it
	}
	return cmpInt(s, p.op, p.threshold)
}
func (p severityPred) cost() int { return costSeverity }

type jsonStrPred struct{ path, op, want string }

func (p jsonStrPred) match(raw []byte, _, _ string) bool {
	r := gjson.GetBytes(raw, p.path)
	if !r.Exists() {
		return false
	}
	return cmpStr(r.String(), p.op, p.want)
}
func (p jsonStrPred) cost() int { return costJSON }

type jsonNumPred struct {
	path, op string
	want     float64
}

func (p jsonNumPred) match(raw []byte, _, _ string) bool {
	r := gjson.GetBytes(raw, p.path)
	if !r.Exists() || r.Type != gjson.Number {
		return false
	}
	return cmpFloat(r.Num, p.op, p.want)
}
func (p jsonNumPred) cost() int { return costJSON }

type logfmtPred struct{ key, op, want string }

func (p logfmtPred) match(raw []byte, _, _ string) bool {
	v, ok := scanLogfmt(raw, p.key)
	if !ok {
		return false
	}
	return cmpStr(v, p.op, p.want)
}
func (p logfmtPred) cost() int { return costLogfmt }

type constPred struct{ v bool }

func (p constPred) match([]byte, string, string) bool { return p.v }
func (p constPred) cost() int                         { return 0 }

type andPred struct {
	kids []predicate
	c    int
}

func (p *andPred) match(raw []byte, stream, service string) bool {
	for _, k := range p.kids {
		if !k.match(raw, stream, service) {
			return false
		}
	}
	return true
}
func (p *andPred) cost() int { return p.c }

type orPred struct {
	kids []predicate
	c    int
}

func (p *orPred) match(raw []byte, stream, service string) bool {
	for _, k := range p.kids {
		if k.match(raw, stream, service) {
			return true
		}
	}
	return false
}
func (p *orPred) cost() int { return p.c }

type notPred struct{ x predicate }

func (p notPred) match(raw []byte, stream, service string) bool {
	return !p.x.match(raw, stream, service)
}
func (p notPred) cost() int { return p.x.cost() }

func cmpInt(a int, op string, b int) bool {
	switch op {
	case "==":
		return a == b
	case "!=":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func cmpFloat(a float64, op string, b float64) bool {
	switch op {
	case "==":
		return a == b
	case "!=":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func cmpStr(a, op, b string) bool {
	if op == "!=" {
		return a != b
	}
	return a == b // compile guarantees only == / != reach here
}

// sortByCost reorders commutative operands so the cheapest gates the rest.
// Stable so equal-cost predicates keep the author's order.
func sortByCost(kids []predicate) int {
	sort.SliceStable(kids, func(i, j int) bool { return kids[i].cost() < kids[j].cost() })
	total := 0
	for _, k := range kids {
		total += k.cost()
	}
	return total
}
