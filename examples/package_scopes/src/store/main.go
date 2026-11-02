package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "")
	label := flag.String("label", "default", "")
	seedPath := flag.String("seed", "", "JSON file: key -> value")
	audit := flag.Bool("audit", false, "")
	flag.Parse()

	b, err := os.ReadFile(*seedPath)
	if err != nil {
		log.Fatal(err)
	}
	var seed map[string]string
	if err := json.Unmarshal(b, &seed); err != nil {
		log.Fatal(err)
	}
	keys := make([]string, 0, len(seed))
	for k := range seed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+seed[k])
	}

	http.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "store=%s audit=%v keys=%s\n", *label, *audit, strings.Join(pairs, ","))
	})
	fmt.Printf("store %s up %s\n", *label, *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
