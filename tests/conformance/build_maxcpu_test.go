//go:build conformance_go

package conformance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestBuildMaxCPU_serializesBuildsAndPropagatesQuota(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")
	p.CopyTree("golden/go/echo", "src/svc2")

	probeSrc := `//go:build ignore

package main

import (
	"fmt"
	"os"
)

func main() {
	for _, k := range []string{"CARGO_BUILD_JOBS", "BUNDLE_JOBS", "MAKEFLAGS", "CMAKE_BUILD_PARALLEL_LEVEL", "GOMAXPROCS"} {
		fmt.Printf("%s=%s\n", k, os.Getenv(k))
	}
}
`
	p.WriteFile("src/svc1/probe.go", probeSrc)
	p.WriteFile("src/svc2/probe.go", probeSrc)

	timelinePath := filepath.Join(p.Dir(), "build-timeline.log")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src { path = "./src/svc1" }

  build {
    cmd = ["/bin/sh", "-c", "echo svc1:enter >> '`+timelinePath+`'; sleep 0.3; go run ./probe.go > \"${fs::state()}/svc1-build.txt\"; go build -o \"${fs::bin()}/svc1\" .; echo svc1:exit >> '`+timelinePath+`'"]
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}

service "go" "svc2" {
  src { path = "./src/svc2" }

  build {
    env = { CARGO_BUILD_JOBS = "1" }
    cmd = ["/bin/sh", "-c", "echo svc2:enter >> '`+timelinePath+`'; sleep 0.3; go run ./probe.go > \"${fs::state()}/svc2-build.txt\"; go build -o \"${fs::bin()}/svc2\" .; echo svc2:exit >> '`+timelinePath+`'"]
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc2", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`)

	t.Cleanup(func() { dumpAlphaLog(t, p.AlphaLogPath()) })

	res := p.Zordon("--build.maxcpu=3", "start", "--timeout", "180s", "--alpha-log", p.AlphaLogPath()).
		WithTimeout(200 * time.Second).
		Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon start exit=%d, want 0\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	rawTimeline, err := os.ReadFile(timelinePath)
	if err != nil {
		t.Fatalf("read timeline: %v", err)
	}
	events := strings.Fields(strings.TrimSpace(string(rawTimeline)))
	if len(events) != 4 {
		t.Fatalf("timeline = %v, want 4 events", events)
	}
	validOrder := (events[0] == "svc1:enter" && events[1] == "svc1:exit" && events[2] == "svc2:enter" && events[3] == "svc2:exit") ||
		(events[0] == "svc2:enter" && events[1] == "svc2:exit" && events[2] == "svc1:enter" && events[3] == "svc1:exit")
	if !validOrder {
		t.Fatalf("builds overlapped instead of serializing on build slot: %v", events)
	}

	stateDir := filepath.Join(p.Dir(), "workspaces", "main")
	svc1Report := readBuildProbeReport(t, filepath.Join(stateDir, "svc1-build.txt"))
	assertProbeMap(t, "svc1", svc1Report, map[string]string{
		"CARGO_BUILD_JOBS":           "3",
		"BUNDLE_JOBS":                "3",
		"MAKEFLAGS":                  "-j3",
		"CMAKE_BUILD_PARALLEL_LEVEL": "3",
		"GOMAXPROCS":                 "3",
	})

	svc2Report := readBuildProbeReport(t, filepath.Join(stateDir, "svc2-build.txt"))
	assertProbeMap(t, "svc2", svc2Report, map[string]string{
		"CARGO_BUILD_JOBS":           "1",
		"BUNDLE_JOBS":                "3",
		"MAKEFLAGS":                  "-j3",
		"CMAKE_BUILD_PARALLEL_LEVEL": "3",
		"GOMAXPROCS":                 "3",
	})

	alphaLog, err := os.ReadFile(p.AlphaLogPath())
	if err != nil {
		t.Fatalf("read alpha log: %v", err)
	}
	if !strings.Contains(string(alphaLog), "build.maxcpu=3") {
		t.Errorf("alpha log missing build.maxcpu=3:\n%s", alphaLog)
	}
	if !strings.Contains(string(alphaLog), "waiting for build slot") {
		t.Errorf("alpha log missing 'waiting for build slot':\n%s", alphaLog)
	}
}

func readBuildProbeReport(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

func assertProbeMap(t *testing.T, label string, got, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %s = %q, want %q (full=%v)", label, k, got[k], w, got)
		}
	}
}
