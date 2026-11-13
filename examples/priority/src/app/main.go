package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	probe := flag.Bool("probe", false, "print POSIX nice value and exit")
	flag.Parse()

	if *probe {
		n, err := selfNice()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("NICE=%d\n", n)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		n, err := selfNice()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "nice=%d\n", n)
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

func selfNice() (int, error) {
	raw, err := syscall.Getpriority(syscall.PRIO_PROCESS, syscall.Getpid())
	if err != nil {
		return 0, err
	}
	if runtime.GOOS == "linux" {
		return 20 - raw, nil
	}
	return raw, nil
}
