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
