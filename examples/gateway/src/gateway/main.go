package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

type route struct {
	Prefix   string `json:"prefix"`
	Upstream string `json:"upstream"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "")
	routes := flag.String("routes", "", "JSON file: key -> {prefix, upstream}")
	accessLog := flag.Bool("access-log", false, "")
	flag.Parse()

	b, err := os.ReadFile(*routes)
	if err != nil {
		log.Fatal(err)
	}
	var table map[string]route
	if err := json.Unmarshal(b, &table); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	for key, r := range table {
		target, err := url.Parse("http://" + r.Upstream)
		if err != nil {
			log.Fatalf("route %s: %v", key, err)
		}
		mux.Handle(r.Prefix, httputil.NewSingleHostReverseProxy(target))
		fmt.Printf("route %s: %s -> %s\n", key, r.Prefix, r.Upstream)
	}

	var h http.Handler = mux
	if *accessLog {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("access %s %s\n", r.Method, r.URL.Path)
			mux.ServeHTTP(w, r)
		})
	}
	fmt.Printf("gateway up %s\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, h))
}
