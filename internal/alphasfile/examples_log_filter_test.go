package alphasfile

import (
	"os"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/logfilter"
)

// Regression oracle for examples/log_filter: the log { filter } block must
// resolve and, once compiled, drop exactly the intended lines. Pure Compile —
// no build, no spawn — so it also pins that the heredoc's `\t` survives HCL
// and reaches the matcher as a real tab (the ruby-backtrace gate depends on
// it).
func TestExampleLogFilterResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/log_filter/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		StateDir: "/repo/examples/log_filter/workspaces/main",
	}
	af, err := Compile("/repo/examples/log_filter/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	app := svcByName(af, "app")
	if app == nil || app.Runtime.Log == nil || app.Runtime.Log.Filter == "" {
		t.Fatalf("app must carry a log filter: %+v", app)
	}
	f, err := logfilter.Compile(app.Runtime.Log.Filter)
	if err != nil {
		t.Fatalf("resolved filter does not compile: %v", err)
	}

	cases := map[string]struct {
		line string
		drop bool
	}{
		"ruby backtrace": {"\tfrom /app/x.rb:1:in `run'", true},
		"json debug":     {`{"level":"debug","msg":"x"}`, true},
		"json error":     {`{"level":"error","msg":"x"}`, false},
		"plain kept":     {"KEEPME plain notice", false},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := f.Drop([]byte(c.line), "stdout", "app"); got != c.drop {
				t.Fatalf("Drop(%q) = %v, want %v", c.line, got, c.drop)
			}
		})
	}
}
