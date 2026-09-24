// Command auth_api is one of the module services of examples/modules; the
// module block, not the binary, decides its identity (auth/api).
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	name := flag.String("name", "", "display name (module/name)")
	upstream := flag.String("upstream", "", "label=addr pairs this service talks to")
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "service=%s\nupstream=%s\n", *name, *upstream)
	})
	fmt.Printf("%s: listening on %s (upstream=%s)\n", *name, *addr, *upstream)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, *name+":", err)
		os.Exit(1)
	}
}
