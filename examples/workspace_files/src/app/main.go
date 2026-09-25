// Command app is the single service of examples/workspace_files. The example
// is about what the top-level workspace{} block writes into the workspace
// directory before anything starts, so this service exists only to give the
// branch template something to name.
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
	flag.Parse()

	cwd, _ := os.Getwd()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from examples/workspace_files\nservice=%s\ncheckout=%s\n", service, cwd)
	})
	fmt.Printf("%s: listening on %s (checkout=%s)\n", service, *addr, cwd)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, service+":", err)
		os.Exit(1)
	}
}
