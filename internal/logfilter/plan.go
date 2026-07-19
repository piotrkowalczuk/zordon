package logfilter

import (
	"fmt"
	"regexp"
	"strings"
)

// compileBool turns a boolean-valued AST node into a predicate, flattening
// and sorting and/or operands by cost so the cheapest condition gates the
// rest (the "condition tree starts from the simplest query" property).
func compileBool(n node) (predicate, error) {
	switch v := n.(type) {
	case *binNode:
		if v.op == "and" || v.op == "or" {
			return compileAndOr(v)
		}
		return compileCmp(v)
	case *notNode:
		x, err := compileBool(v.x)
		if err != nil {
			return nil, err
		}
		return notPred{x: x}, nil
	case *callNode:
		return compileBoolCall(v)
	case *boolLitNod:
		return constPred{v: v.v}, nil
	case *identNode:
		return nil, fmt.Errorf("%q must be part of a comparison (e.g. %s == \"…\")", v.name, v.name)
	default:
		return nil, fmt.Errorf("expected a condition")
	}
}

func compileAndOr(v *binNode) (predicate, error) {
	op := v.op
	var flat []node
	var collect func(n node)
	collect = func(n node) {
		if b, ok := n.(*binNode); ok && b.op == op {
			collect(b.l)
			collect(b.r)
			return
		}
		flat = append(flat, n)
	}
	collect(v)

	kids := make([]predicate, 0, len(flat))
	for _, fn := range flat {
		k, err := compileBool(fn)
		if err != nil {
			return nil, err
		}
		kids = append(kids, k)
	}
	total := sortByCost(kids)
	if op == "and" {
		return &andPred{kids: kids, c: total}, nil
	}
	return &orPred{kids: kids, c: total}, nil
}

func compileBoolCall(v *callNode) (predicate, error) {
	switch v.name {
	case "contains", "hasPrefix", "hasSuffix":
		s, err := lineStrArg2(v)
		if err != nil {
			return nil, err
		}
		mode := byte('c')
		switch v.name {
		case "hasPrefix":
			mode = 'p'
		case "hasSuffix":
			mode = 's'
		}
		return substrPred{mode: mode, sub: []byte(s)}, nil
	case "matches":
		s, err := lineStrArg2(v)
		if err != nil {
			return nil, err
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("matches: %w", err)
		}
		return &regexPred{re: re, prefix: literalPrefix(re, s)}, nil
	case "json", "logfmt", "severity":
		return nil, fmt.Errorf("%s(...) must be compared, e.g. %s(line, \"level\") == \"error\"", v.name, v.name)
	default:
		return nil, fmt.Errorf("unknown function %q", v.name)
	}
}

// fieldInfo classifies one side of a comparison as an extractable field.
type fieldInfo struct {
	kind byte   // 'm' meta, 'S' severity, 'j' json, 'l' logfmt
	meta byte   // 's' stream / 'v' service (kind 'm')
	arg  string // json path / logfmt key
}

func compileCmp(v *binNode) (predicate, error) {
	lf, err := classifyField(v.l)
	if err != nil {
		return nil, err
	}
	rf, err := classifyField(v.r)
	if err != nil {
		return nil, err
	}
	switch {
	case lf != nil && rf == nil:
		return buildCmp(lf, v.op, v.r)
	case rf != nil && lf == nil:
		return buildCmp(rf, flipOp(v.op), v.l)
	case lf != nil && rf != nil:
		return nil, fmt.Errorf("cannot compare two fields to each other")
	default:
		return nil, fmt.Errorf("comparison needs a field (stream, service, json(...), logfmt(...), severity(...)) on one side")
	}
}

func buildCmp(f *fieldInfo, op string, lit node) (predicate, error) {
	switch f.kind {
	case 'm':
		if op != "==" && op != "!=" {
			return nil, fmt.Errorf("stream/service support only == and !=")
		}
		s, ok := strLit(lit)
		if !ok {
			return nil, fmt.Errorf("compare stream/service to a string")
		}
		return metaPred{field: f.meta, want: s, ne: op == "!="}, nil
	case 'S':
		th, ok := sevThreshold(lit)
		if !ok {
			return nil, fmt.Errorf("compare severity(line) to a level (WARN, ERROR, …) or a number")
		}
		return severityPred{op: op, threshold: th}, nil
	case 'l':
		if op != "==" && op != "!=" {
			return nil, fmt.Errorf("logfmt(...) supports only == and !=")
		}
		s, ok := strLit(lit)
		if !ok {
			return nil, fmt.Errorf("compare logfmt(...) to a string")
		}
		return logfmtPred{key: f.arg, op: op, want: s}, nil
	case 'j':
		if s, ok := strLit(lit); ok {
			if op != "==" && op != "!=" {
				return nil, fmt.Errorf("json(...) string comparison supports only == and !=")
			}
			return jsonStrPred{path: f.arg, op: op, want: s}, nil
		}
		if num, ok := numLit(lit); ok {
			return jsonNumPred{path: f.arg, op: op, want: num}, nil
		}
		return nil, fmt.Errorf("compare json(...) to a string or a number")
	}
	return nil, fmt.Errorf("unsupported comparison")
}

func classifyField(n node) (*fieldInfo, error) {
	switch v := n.(type) {
	case *identNode:
		switch v.name {
		case "stream":
			return &fieldInfo{kind: 'm', meta: 's'}, nil
		case "service":
			return &fieldInfo{kind: 'm', meta: 'v'}, nil
		}
		return nil, nil
	case *callNode:
		switch v.name {
		case "severity":
			if err := lineArg1(v); err != nil {
				return nil, err
			}
			return &fieldInfo{kind: 'S'}, nil
		case "json":
			path, err := lineStrArg2(v)
			if err != nil {
				return nil, err
			}
			return &fieldInfo{kind: 'j', arg: path}, nil
		case "logfmt":
			key, err := lineStrArg2(v)
			if err != nil {
				return nil, err
			}
			return &fieldInfo{kind: 'l', arg: key}, nil
		}
	}
	return nil, nil
}

func lineArg1(v *callNode) error {
	if len(v.args) != 1 {
		return fmt.Errorf("%s(line) takes 1 argument", v.name)
	}
	if id, ok := v.args[0].(*identNode); !ok || id.name != "line" {
		return fmt.Errorf("%s: argument must be line", v.name)
	}
	return nil
}

func lineStrArg2(v *callNode) (string, error) {
	if len(v.args) != 2 {
		return "", fmt.Errorf("%s(line, \"…\") takes 2 arguments", v.name)
	}
	if id, ok := v.args[0].(*identNode); !ok || id.name != "line" {
		return "", fmt.Errorf("%s: first argument must be line", v.name)
	}
	s, ok := v.args[1].(*strNode)
	if !ok {
		return "", fmt.Errorf("%s: second argument must be a string", v.name)
	}
	return s.v, nil
}

func strLit(n node) (string, bool) {
	s, ok := n.(*strNode)
	if !ok {
		return "", false
	}
	return s.v, true
}

func numLit(n node) (float64, bool) {
	switch v := n.(type) {
	case *intNode:
		return float64(v.v), true
	case *floatNode:
		return v.v, true
	}
	return 0, false
}

func sevThreshold(n node) (int, bool) {
	switch v := n.(type) {
	case *intNode:
		return int(v.v), true
	case *identNode:
		return sevConst(v.name)
	}
	return 0, false
}

func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

// literalPrefix returns the bytes a match must begin the line with, used as a
// cheap HasPrefix pre-gate before the (expensive) regex engine runs. It is a
// valid line-start gate only when the pattern is anchored (^): otherwise the
// match — and its literal prefix — may sit mid-line. The engine's own
// LiteralPrefix already yields "" for alternation, wildcards, and optional
// leading chars, so a non-empty result on an anchored pattern is exactly the
// mandatory line prefix. Returns nil when no safe gate exists.
func literalPrefix(re *regexp.Regexp, pat string) []byte {
	if !strings.HasPrefix(pat, "^") || strings.ContainsRune(pat, '|') {
		return nil
	}
	lit, _ := re.LiteralPrefix()
	if lit == "" {
		return nil
	}
	return []byte(lit)
}
