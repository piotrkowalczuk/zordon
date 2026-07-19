package logfilter

import (
	"fmt"
	"testing"
)

// benchCorpus is a realistic mix of what a chatty service emits, as []byte
// lines (what bufio.Scanner.Bytes hands us): JSON info/error, ruby backtrace
// noise, and plain text.
var (
	benchCorpus [][]byte
	benchBytes  int64
)

func init() {
	const n = 20000
	benchCorpus = make([][]byte, 0, n)
	for i := range n {
		var s string
		switch i % 5 {
		case 0, 1:
			s = fmt.Sprintf(`{"ts":"2026-07-19T12:34:56.%03dZ","level":"info","service":"api","msg":"handled path=/v1/resource/%d status=200 dur=%dms"}`, i%1000, i, i%97)
		case 2:
			s = fmt.Sprintf(`{"ts":"2026-07-19T12:34:56.%03dZ","level":"error","service":"db","msg":"connection reset conn=%d"}`, i%1000, i)
		case 3:
			s = fmt.Sprintf("\tfrom /Users/dev/.gem/ruby/3.3.0/gems/puma/lib/puma/server.rb:%d:in `block in run'", 100+i%400)
		case 4:
			s = fmt.Sprintf("[2026-07-19T12:34:56Z] INFO listening on 127.0.0.1:8080 (worker %d)", i%8)
		}
		b := []byte(s)
		benchCorpus = append(benchCorpus, b)
		benchBytes += int64(len(b))
	}
}

var benchSink int

func benchFilter(b *testing.B, expr string) {
	f, err := Compile(expr)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(benchBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		dropped := 0
		for _, ln := range benchCorpus {
			if f.Drop(ln, "stdout", "api") {
				dropped++
			}
		}
		benchSink += dropped
	}
}

func BenchmarkDrop_Substring(b *testing.B) { benchFilter(b, `contains(line, "\tfrom ")`) }
func BenchmarkDrop_HasPrefix(b *testing.B) { benchFilter(b, `hasPrefix(line, "\tfrom ")`) }
func BenchmarkDrop_Regex(b *testing.B)     { benchFilter(b, `matches(line, "^\tfrom ")`) }
func BenchmarkDrop_Severity(b *testing.B)  { benchFilter(b, `severity(line) < WARN`) }
func BenchmarkDrop_JSONField(b *testing.B) {
	benchFilter(b, `contains(line, "error") and json(line, "level") == "error"`)
}
func BenchmarkDrop_Combined(b *testing.B) {
	benchFilter(b, `stream == "stderr" or contains(line, "\tfrom ") or json(line, "level") == "error"`)
}
