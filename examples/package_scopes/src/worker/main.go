package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "")
	name := flag.String("name", "service", "")
	prefix := flag.String("prefix", "/", "")
	storeURL := flag.String("store", "", "base URL of the store")
	flag.Parse()

	client := &http.Client{Timeout: 5 * time.Second}
	http.HandleFunc(*prefix, func(w http.ResponseWriter, _ *http.Request) {
		resp, err := client.Get(*storeURL + "/info")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(w, "%s ok (%s)\n", *name, strings.TrimSpace(string(body)))
	})
	fmt.Printf("%s up %s -> %s\n", *name, *addr, *storeURL)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
