package main

import (
	"flag"
	"fmt"
	"net/http"
	"sync/atomic"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "")
	flag.Parse()

	var hits atomic.Int64

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "go-delve-example ok")
	})

	// /hit is the interesting endpoint to break on from a delve client:
	//   (dlv) break main.handle
	// Each call mutates `hits` and branches on its parity, so a single
	// breakpoint demonstrates `print hits`, `next`, and conditional
	// flow without needing more than one source file.
	http.HandleFunc("/hit", handle(&hits))

	fmt.Printf("up %s\n", *addr)
	_ = http.ListenAndServe(*addr, nil)
}

func handle(hits *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n%2 == 0 {
			fmt.Fprintf(w, "even hit=%d\n", n)
			return
		}
		fmt.Fprintf(w, "odd hit=%d\n", n)
	}
}
