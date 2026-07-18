// backend is a trivial peer whose only job here is to exist so the
// Alphasfile can drop a config fragment into gateway's etc dir via
// fs::service::etc(service.go.gateway).
package main

import (
	"flag"
	"fmt"
	"net/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	flag.Parse()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	fmt.Printf("backend up on %s\n", *addr)
	_ = http.ListenAndServe(*addr, nil)
}
