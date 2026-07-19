package logfilter

import (
	"regexp"
	"testing"
)

const (
	jsonErr    = `{"level":"error","msg":"boom","http":{"status":500,"path":"/healthz"}}`
	logfmtWarn = `ts=2026 level=warn component=gossip msg="hi there"`
	logfmtInfo = `ts=2026 level=info component=api msg="ok"`
	rubyLine   = "\tfrom /Users/x/foo.rb:1:in `run'"
	plainLine  = "[2026] INFO listening on :8080"
)

func TestFilter_Drop(t *testing.T) {
	cases := map[string]struct {
		filter  string
		line    string
		stream  string
		service string
		drop    bool
	}{
		"empty keeps everything":       {"", jsonErr, "stderr", "db", false},
		"contains hit":                 {`contains(line, "boom")`, jsonErr, "stdout", "db", true},
		"contains miss":                {`contains(line, "nope")`, jsonErr, "stdout", "db", false},
		"hasPrefix hit":                {`hasPrefix(line, "\tfrom ")`, rubyLine, "stderr", "db", true},
		"hasPrefix miss":               {`hasPrefix(line, "boom")`, jsonErr, "stdout", "db", false},
		"hasSuffix hit":                {`hasSuffix(line, ":8080")`, plainLine, "stdout", "db", true},
		"matches anchored hit":         {`matches(line, "^\tfrom ")`, rubyLine, "stderr", "db", true},
		"matches miss":                 {`matches(line, "^ERROR ")`, plainLine, "stdout", "db", false},
		"stream eq hit":                {`stream == "stderr"`, jsonErr, "stderr", "db", true},
		"stream eq miss":               {`stream == "stderr"`, jsonErr, "stdout", "db", false},
		"service eq hit":               {`service == "db"`, jsonErr, "stdout", "db", true},
		"service ne hit":               {`service != "db"`, jsonErr, "stdout", "api", true},
		"severity below warn drops":    {`severity(line) < WARN`, logfmtInfo, "stdout", "api", true},
		"severity error not below":     {`severity(line) < WARN`, jsonErr, "stderr", "db", false},
		"severity ge warn drops error": {`severity(line) >= WARN`, jsonErr, "stderr", "db", true},
		"severity int threshold":       {`severity(line) >= 4`, jsonErr, "stderr", "db", true},
		"severity unknown keeps":       {`severity(line) < WARN`, plainLine, "stdout", "api", false},
		"json num ge hit":              {`json(line, "http.status") >= 500`, jsonErr, "stderr", "db", true},
		"json num gt miss":             {`json(line, "http.status") > 500`, jsonErr, "stderr", "db", false},
		"json str eq hit":              {`json(line, "level") == "error"`, jsonErr, "stderr", "db", true},
		"json str literal on left":     {`"error" == json(line, "level")`, jsonErr, "stderr", "db", true},
		"json str ne miss":             {`json(line, "level") != "error"`, jsonErr, "stderr", "db", false},
		"json absent field keeps":      {`json(line, "nope") == "x"`, jsonErr, "stderr", "db", false},
		"logfmt eq hit":                {`logfmt(line, "component") == "gossip"`, logfmtWarn, "stdout", "db", true},
		"logfmt ne miss":               {`logfmt(line, "component") != "gossip"`, logfmtWarn, "stdout", "db", false},
		"and both true":                {`contains(line, "boom") and stream == "stderr"`, jsonErr, "stderr", "db", true},
		"and one false":                {`contains(line, "boom") and stream == "stdout"`, jsonErr, "stderr", "db", false},
		"or one true":                  {`stream == "stdout" or contains(line, "boom")`, jsonErr, "stderr", "db", true},
		"not over hit":                 {`not contains(line, "boom")`, jsonErr, "stderr", "db", false},
		"not over miss":                {`not contains(line, "nope")`, jsonErr, "stderr", "db", true},
		"combined priority + gate":     {`contains(line, "level") and severity(line) < WARN`, logfmtInfo, "stdout", "api", true},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			f, err := Compile(c.filter)
			if err != nil {
				t.Fatalf("compile %q: %v", c.filter, err)
			}
			got := f.Drop([]byte(c.line), c.stream, c.service)
			if got != c.drop {
				t.Fatalf("Drop(%q) filter=%q = %v, want %v", c.line, c.filter, got, c.drop)
			}
		})
	}
}

func TestCompile_error(t *testing.T) {
	cases := map[string]string{
		"uncompared json":     `json(line, "level")`,
		"uncompared severity": `severity(line)`,
		"bare identifier":     `stream`,
		"contains arity":      `contains(line)`,
		"unknown function":    `foo(line, "x")`,
		"bad stream op":       `stream < "x"`,
		"json bool literal":   `json(line, "a") == true`,
		"unterminated paren":  `contains(line, "x"`,
		"first arg not line":  `contains(msg, "x")`,
		"two fields":          `stream == service`,
	}
	for hint, src := range cases {
		t.Run(hint, func(t *testing.T) {
			if err := Validate(src); err == nil {
				t.Fatalf("Validate(%q) = nil, want error", src)
			}
		})
	}
}

// TestFilter_costPlanner is white-box: it verifies the compiler reorders
// and/or operands so the cheapest predicate is evaluated first, regardless of
// how the author wrote them.
func TestFilter_costPlanner(t *testing.T) {
	f, err := Compile(`severity(line) < WARN and stream == "stderr"`)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := f.root.(*andPred)
	if !ok {
		t.Fatalf("root = %T, want *andPred", f.root)
	}
	if len(root.kids) != 2 {
		t.Fatalf("kids = %d, want 2", len(root.kids))
	}
	if _, ok := root.kids[0].(metaPred); !ok {
		t.Fatalf("kids[0] = %T, want metaPred (cost 0) hoisted first", root.kids[0])
	}
	if _, ok := root.kids[1].(severityPred); !ok {
		t.Fatalf("kids[1] = %T, want severityPred", root.kids[1])
	}
	if root.kids[0].cost() > root.kids[1].cost() {
		t.Fatalf("kids not sorted by ascending cost: %d > %d", root.kids[0].cost(), root.kids[1].cost())
	}
}

func TestScanSeverity(t *testing.T) {
	cases := map[string]struct {
		line string
		want int
	}{
		"json error":   {jsonErr, 4},
		"logfmt warn":  {logfmtWarn, 3},
		"logfmt info":  {logfmtInfo, 2},
		"severity key": {`{"severity":"debug"}`, 1},
		"syslog crit":  {`level=crit boom`, 5},
		"no level":     {plainLine, severityUnknown},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := scanSeverity([]byte(c.line)); got != c.want {
				t.Fatalf("scanSeverity(%q) = %d, want %d", c.line, got, c.want)
			}
		})
	}
}

func TestScanLogfmt(t *testing.T) {
	cases := map[string]struct {
		key  string
		want string
		ok   bool
	}{
		"bare value":   {"component", "gossip", true},
		"quoted value": {"msg", "hi there", true},
		"absent":       {"nope", "", false},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, ok := scanLogfmt([]byte(logfmtWarn), c.key)
			if ok != c.ok || got != c.want {
				t.Fatalf("scanLogfmt(%q) = (%q,%v), want (%q,%v)", c.key, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestLiteralPrefix(t *testing.T) {
	cases := map[string]struct {
		pat  string
		want string
	}{
		"anchored literal": {`^ERROR `, "ERROR "},
		"anchored tab":     {"^\tfrom ", "\tfrom "},
		"wildcard breaks":  {`^foo.*bar`, ""}, // engine won't promise past .*
		"optional breaks":  {`^ab?c`, ""},
		"unanchored":       {`foo`, ""}, // match may sit mid-line
		"alternation":      {`^foo|^bar`, ""},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			re := regexp.MustCompile(c.pat)
			if got := string(literalPrefix(re, c.pat)); got != c.want {
				t.Fatalf("literalPrefix(%q) = %q, want %q", c.pat, got, c.want)
			}
		})
	}
}
