package main

import (
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestWithBaseline_nothingFromTheHost(t *testing.T) {
	home := zfs.StateDir("/z")
	got := envMap(withBaseline(nil, home))
	want := map[string]string{
		"GOPATH":     home.Join("go"),
		"GOMODCACHE": home.Join("go", "pkg", "mod"),
		"GOCACHE":    home.Join("go", "cache"),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if v, ok := got["PATH"]; ok {
		t.Errorf("PATH = %q; without one, a bare command resolves against alpha's PATH", v)
	}
}

func TestWithBaseline_hostValuesWin(t *testing.T) {
	got := envMap(withBaseline([]string{"PATH=/host/bin", "HOME=/Users/me"}, "/z"))
	if got["PATH"] != "/host/bin" {
		t.Errorf("PATH = %q, a passed PATH must win", got["PATH"])
	}
	for _, k := range []string{"GOPATH", "GOMODCACHE", "GOCACHE"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %q, but with HOME passed Go derives its caches itself", k, v)
		}
	}
}

func TestWithBaseline_explicitGoCacheWins(t *testing.T) {
	got := envMap(withBaseline([]string{"GOCACHE=/mine"}, "/z"))
	if got["GOCACHE"] != "/mine" || got["GOPATH"] != zfs.StateDir("/z").Join("go") {
		t.Errorf("env = %v", got)
	}
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}
