// Command echo is a conformance-test fixture: a service that mirrors
// back its execution context (env, cwd, argv, runtime version) as
// JSON so tests can assert on what zordon actually passed it.
//
// The harness copies this directory into a test project, writes an
// Alphasfile that references it as a service, runs zordon, and hits
// GET / to read back the state the spawned process saw.
package main

import (
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "HTTP listen address")
	// Generic arbitrary-string flag for tests that need to verify argv
	// interpolation reaches the binary. Unused by the echo logic itself;
	// it shows up in os.Args (which the JSON response mirrors).
	_ = flag.String("tag", "", "echoed back via os.Args (test hook)")
	// Sleep before binding so HTTP readiness probes can't pass for the
	// given duration — widens the bringup window for parent-death
	// conformance tests that need a stable interval in which to kill
	// the zordon spawner.
	readyDelay := flag.Duration("ready-delay", 0, "sleep before HTTP listen (extends bringup window)")
	flag.Parse()

	if *readyDelay > 0 {
		time.Sleep(*readyDelay)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cwd, _ := os.Getwd()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"env":             envMap(),
			"cwd":             cwd,
			"argv":            os.Args,
			"runtime_version": runtime.Version(),
		})
	})

	if err := http.ListenAndServe(*addr, mux); err != nil {
		// One-shot daemon: any listen error is fatal — the test will
		// observe a connection refused on GET / and fail with a
		// useful message.
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

func envMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}
