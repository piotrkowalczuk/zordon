package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "")
	flag.Parse()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	http.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		e := os.Environ()
		sort.Strings(e)
		fmt.Fprint(w, strings.Join(e, "\n"))
	})
	fmt.Printf("up %s\n", *addr)
	_ = http.ListenAndServe(*addr, nil)
}
