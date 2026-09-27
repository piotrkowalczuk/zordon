package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "")
	greeting := flag.String("greeting", "hello", "")
	flag.Parse()
	http.HandleFunc("/orders/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "orders ok: %s\n", *greeting)
	})
	fmt.Printf("orders up %s\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
