package main

import (
	"flag"
	"fmt"
	"net/http"
)

func main() {
	a := flag.String("addr", "127.0.0.1:8080", "")
	flag.Parse()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "kafka ok") })
	fmt.Printf("up %s\n", *a)
	_ = http.ListenAndServe(*a, nil)
}
