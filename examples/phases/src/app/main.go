// Command app makes phase-scoped env visible at runtime. BuiltBy is set
// via ldflags from a variable that only exists in the build phase env
// (`build { env }`), proving build env reached the compiler; the rest is
// read live from the process env to show what `runtime {}` / `agent {}`
// injected (and that build-only vars do NOT leak into the running
// process).
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
)

// BuiltBy is overridden at build time via
// `-ldflags "-X main.BuiltBy=$BUILD_TAG"`, where BUILD_TAG comes from
// the service's `build { env {} }`.
var BuiltBy = "unset"

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	flag.Parse()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "builtby=%s\n", BuiltBy)
		fmt.Fprintf(w, "RUNTIME_ONLY=%s\n", os.Getenv("RUNTIME_ONLY"))
		fmt.Fprintf(w, "VERBOSITY=%s\n", os.Getenv("VERBOSITY"))
		// Empty: BUILD_TAG lives only in the build phase env.
		fmt.Fprintf(w, "BUILD_TAG_at_runtime=%s\n", os.Getenv("BUILD_TAG"))
	})
	fmt.Printf("up %s\n", *addr)
	_ = http.ListenAndServe(*addr, nil)
}
