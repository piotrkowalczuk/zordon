package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	name := flag.String("name", "app", "service name")
	probe := flag.Bool("probe", false, "print build quota env, then exit")
	flag.Parse()

	if *probe {
		for _, k := range []string{
			"CARGO_BUILD_JOBS",
			"BUNDLE_JOBS",
			"MAKEFLAGS",
			"CMAKE_BUILD_PARALLEL_LEVEL",
			"GOMAXPROCS",
		} {
			fmt.Printf("%s=%s\n", k, os.Getenv(k))
		}
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "service=%s\n", *name)
	})
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
