package main

import (
	"flag"
	"fmt"
	"net/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "")
	name := flag.String("name", "app", "")
	upstream := flag.String("upstream", "", "")
	flag.Parse()
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "service=%s upstream=%s\n", *name, *upstream)
	})
	_ = http.ListenAndServe(*addr, nil)
}
