// gateway reads its listen port from the generated config at -conf
// (materialized by zordon into fs::etc()) and keeps runtime state under
// -data (fs::var()) — the two persistent anchors this example demonstrates.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
)

func main() {
	conf := flag.String("conf", "", "generated config path (fs::etc())")
	data := flag.String("data", "", "runtime state dir (fs::var())")
	flag.Parse()

	if *data != "" {
		_ = os.MkdirAll(*data, 0o750)
	}
	port := readPort(*conf)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	fmt.Printf("gateway up on 127.0.0.1:%s (conf=%s data=%s)\n", port, *conf, *data)
	_ = http.ListenAndServe("127.0.0.1:"+port, nil)
}

func readPort(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "0"
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "port="); ok {
			return v
		}
	}
	return "0"
}
