// Command example is a minimal HTTP server used by examples/worktree to
// demonstrate worktree isolation: run it from two worktrees and each
// instance reports its own port, invocation tag (pathhash) and the git
// worktree checkout it was built from — proving the runs don't collide.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	tag := flag.String("tag", "", "invocation tag (pathhash)")
	flag.Parse()

	cwd, _ := os.Getwd()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from examples/worktree\n")
		fmt.Fprintf(w, "tag=%s\n", *tag)
		fmt.Fprintf(w, "addr=%s\n", *addr)
		fmt.Fprintf(w, "checkout=%s\n", cwd)
	})

	fmt.Printf("example: listening on %s (tag=%s, checkout=%s)\n", *addr, *tag, cwd)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "example:", err)
		os.Exit(1)
	}
}
