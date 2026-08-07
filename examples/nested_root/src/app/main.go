// Command app is the single service of examples/nested_root. The example
// asserts zordon's invocation gate: `zordon start` runs only from the project
// root or a workspace dir, never from a plain subdir or from inside a service
// checkout that carries its own Alphasfile — which would otherwise materialize
// a whole nested stack whose branches collide with the real one (issue #73).
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
)

const service = "app"

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	tag := flag.String("tag", "", "invocation tag (fs::hash)")
	flag.Parse()

	cwd, _ := os.Getwd()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from examples/nested_root\nservice=%s\ntag=%s\naddr=%s\ncheckout=%s\n",
			service, *tag, *addr, cwd)
	})
	fmt.Printf("%s: listening on %s (tag=%s, checkout=%s)\n", service, *addr, *tag, cwd)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, service+":", err)
		os.Exit(1)
	}
}
