package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	probePort := flag.Int("probe-port", 0, "probe whether 127.0.0.1:<port> is held during build")
	flag.Parse()

	if *probePort > 0 {
		held := isPortHeld(*probePort)
		fmt.Printf("PORT=%d\nHELD=%t\n", *probePort, held)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "addr=%s\n", *addr)
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

func isPortHeld(port int) bool {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return false
	}
	defer func() { _ = syscall.Close(fd) }()
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	return syscall.Bind(fd, sa) != nil
}
