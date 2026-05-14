package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/codegangsta/cli"
)

var zordonEnvRe = regexp.MustCompile(`ZORDON[A-Z_]*=\S*`)

type listEntry struct {
	pid        int
	pgid       int
	service    string
	alphasFile string
	ppid       int
}

func list(ctx *cli.Context) error {
	entries, err := scanZordonProcs()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no zordon-managed services running")
		return nil
	}

	groups := map[string][]listEntry{}
	for _, e := range entries {
		groups[e.alphasFile] = append(groups[e.alphasFile], e)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for i, af := range keys {
		zordonPID := 0
		if len(groups[af]) > 0 {
			zordonPID = groups[af][0].ppid
		}
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s (zordon pid %d)\n", af, zordonPID)
		sort.Slice(groups[af], func(i, j int) bool { return groups[af][i].service < groups[af][j].service })
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, e := range groups[af] {
			fmt.Fprintf(w, "  %d\t%s\tpgid=%d\n", e.pid, e.service, e.pgid)
		}
		w.Flush()
	}
	return nil
}

func scanZordonProcs() ([]listEntry, error) {
	out, err := exec.Command("ps", "-E", "-axwww", "-o", "pid=,pgid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps failed: %w", err)
	}
	var entries []listEntry
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		envs := map[string]string{}
		for _, m := range zordonEnvRe.FindAllString(line, -1) {
			kv := strings.SplitN(m, "=", 2)
			if len(kv) == 2 {
				envs[kv[0]] = kv[1]
			}
		}
		if envs["ZORDON"] != "1" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		pgid, _ := strconv.Atoi(fields[1])
		ppid, _ := strconv.Atoi(envs["ZORDON_PPID"])
		entries = append(entries, listEntry{
			pid:        pid,
			pgid:       pgid,
			service:    envs["ZORDON_SERVICE"],
			alphasFile: envs["ZORDON_ALPHASFILE"],
			ppid:       ppid,
		})
	}
	return entries, nil
}
