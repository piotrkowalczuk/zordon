package logfilter

import "bytes"

// severityUnknown is returned by scanSeverity when a line carries no
// recognizable level. Filters treat unknown as "keep" (see severityPred).
const severityUnknown = -1

// levelNames are the DSL severity constants (TRACE < DEBUG < … < FATAL),
// usable on the right-hand side of a severity() comparison.
var levelNames = map[string]int{
	"TRACE": 0,
	"DEBUG": 1,
	"INFO":  2,
	"WARN":  3,
	"ERROR": 4,
	"FATAL": 5,
}

func sevConst(name string) (int, bool) {
	v, ok := levelNames[name]
	return v, ok
}

// levelOrdinal maps a level token (lower-cased content, syslog aliases
// included) to the 0..5 scale. Unknown tokens yield severityUnknown.
func levelOrdinal(tok string) int {
	switch tok {
	case "trace":
		return 0
	case "debug":
		return 1
	case "info", "informational", "notice":
		return 2
	case "warn", "warning":
		return 3
	case "error", "err":
		return 4
	case "fatal", "critical", "crit", "panic", "emerg", "emergency", "alert":
		return 5
	}
	return severityUnknown
}

// scanSeverity extracts a level from a raw line without parsing the whole
// line: it locates a known level key (JSON "level":"…", logfmt level=…, or a
// bare severity/lvl key) and maps only that one token. Returns
// severityUnknown when nothing matches.
func scanSeverity(raw []byte) int {
	for _, key := range [][]byte{[]byte("level"), []byte("severity"), []byte("lvl")} {
		if tok, ok := valueAfterKey(raw, key); ok {
			if ord := levelOrdinal(lower(tok)); ord != severityUnknown {
				return ord
			}
		}
	}
	return severityUnknown
}

// valueAfterKey finds key and reads the value token that follows it,
// tolerating both `"key":"value"` (JSON) and `key=value` (logfmt) shapes.
func valueAfterKey(raw, key []byte) ([]byte, bool) {
	from := 0
	for {
		rel := bytes.Index(raw[from:], key)
		if rel < 0 {
			return nil, false
		}
		i := from + rel + len(key)
		// skip separators between key and value: closing quote, colon,
		// equals, spaces, and the value's opening quote.
		for i < len(raw) && (raw[i] == '"' || raw[i] == ':' || raw[i] == '=' || raw[i] == ' ') {
			i++
		}
		start := i
		for i < len(raw) && isLevelChar(raw[i]) {
			i++
		}
		if i > start {
			return raw[start:i], true
		}
		from = from + rel + len(key)
	}
}

// scanLogfmt extracts key=value from a raw line (value may be quoted or run
// to the next space), matching only at a key boundary. Returns false when the
// key is absent.
func scanLogfmt(raw []byte, key string) (string, bool) {
	needle := append([]byte(key), '=')
	from := 0
	for {
		rel := bytes.Index(raw[from:], needle)
		if rel < 0 {
			return "", false
		}
		abs := from + rel
		if abs == 0 || raw[abs-1] == ' ' { // key boundary
			i := abs + len(needle)
			if i < len(raw) && raw[i] == '"' {
				i++
				start := i
				for i < len(raw) && raw[i] != '"' {
					i++
				}
				return string(raw[start:i]), true
			}
			start := i
			for i < len(raw) && raw[i] != ' ' {
				i++
			}
			return string(raw[start:i]), true
		}
		from = abs + len(needle)
	}
}

func isLevelChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func lower(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
