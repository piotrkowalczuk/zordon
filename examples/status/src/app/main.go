package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
)

func main() {
	a := flag.String("addr", "127.0.0.1:8080", "")
	flag.Parse()
	wd, _ := os.Getwd()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "go-example ok cwd=%s\n", wd) })
	fmt.Printf("up %s\n", *a)
	_ = http.ListenAndServe(*a, nil)
}
