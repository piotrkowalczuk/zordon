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
	gateway := flag.String("gateway", "", "base URL of the gateway")
	flag.Parse()
	client := &http.Client{Timeout: 5 * time.Second}
	http.HandleFunc("/billing/", func(w http.ResponseWriter, _ *http.Request) {
		resp, err := client.Get(*gateway + "/orders/")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(w, "billing ok; orders said: %s\n", strings.TrimSpace(string(body)))
	})
	fmt.Printf("billing up %s, gateway %s\n", *addr, *gateway)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
