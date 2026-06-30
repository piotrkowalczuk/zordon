// Command app is the single service of examples/worktree_selfheal. It
// is checked out into a per-service sparse worktree; the example strands
// that worktree mid-init (the `initializing` lock + a half-removed tree)
// and asserts the next `zordon start` rebuilds it instead of reusing the
// broken checkout (issue #38).
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
		fmt.Fprintf(w, "hello from examples/worktree_selfheal\nservice=%s\ntag=%s\naddr=%s\ncheckout=%s\n",
			service, *tag, *addr, cwd)
	})
	fmt.Printf("%s: listening on %s (tag=%s, checkout=%s)\n", service, *addr, *tag, cwd)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, service+":", err)
		os.Exit(1)
	}
}
