// Command serviceA is one of three binaries in the SAME repo —
// examples/worktree is a genuine monorepo (serviceA/B/C share one
// primary). Each gets its own per-service checkout and branch
// (zordon/<worktree>/<service>); they never collide.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
)

const service = "serviceA"

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	tag := flag.String("tag", "", "invocation tag (pathhash)")
	flag.Parse()

	cwd, _ := os.Getwd()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from examples/worktree\nservice=%s\ntag=%s\naddr=%s\ncheckout=%s\n",
			service, *tag, *addr, cwd)
	})
	fmt.Printf("%s: listening on %s (tag=%s, checkout=%s)\n", service, *addr, *tag, cwd)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, service+":", err)
		os.Exit(1)
	}
}
