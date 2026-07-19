package main

import (
	"flag"
	"fmt"
	"net/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "")
	flag.Parse()

	// Emit a representative mix on stdout BEFORE serving. The filter in
	// this example's Alphasfile drops the first two and keeps the rest;
	// "up" is printed last, so once it lands in alpha's log the whole
	// stdout stream has been drained and the drop/keep assertions in
	// example_test.go see a settled log.
	fmt.Println(`{"level":"debug","msg":"DROPME_DEBUG warming cache"}`) // dropped: severity <= DEBUG
	fmt.Println("\tfrom /app/lib/server.rb:42:in `run'")                // dropped: hasPrefix "\tfrom "
	fmt.Println(`{"level":"error","msg":"KEEPME_ERROR upstream down"}`) // kept
	fmt.Println("KEEPME_PLAIN startup notice")                          // kept

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "log-filter ok")
	})
	fmt.Printf("up %s\n", *addr)
	_ = http.ListenAndServe(*addr, nil)
}
