//go:build conformance_go

package conformance_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/daemon"
	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestAlphaProcessTree_inheritsLoweredNicePriority(t *testing.T) {
	parentNice, err := daemon.ProcessNice(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessNice(self): %v", err)
	}
	wantNice := max(parentNice, daemon.DefaultNice)

	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/app")
	p.WriteFile("src/app/nice_probe.go", `//go:build ignore

package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

func main() {
	raw, err := syscall.Getpriority(syscall.PRIO_PROCESS, syscall.Getpid())
	if err != nil {
		panic(err)
	}
	nice := raw
	if runtime.GOOS == "linux" {
		nice = 20 - raw
	}
	fmt.Println(nice)
	_ = os.Stdout.Sync()
}
`)

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "app" {
  src { path = "./src/app" }

  build {
    cmd = ["/bin/sh", "-c", "go run ./nice_probe.go > \"${fs::state()}/build-nice.txt\"; go build -o \"${fs::bin()}/app\" ."]
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/app", "-addr", "127.0.0.1:${self.vars.port}"]

    provision "probe" {
      after = [self.runtime.ready]
      cmd   = "go run ./nice_probe.go > \"${fs::state()}/provision-nice.txt\""
    }
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
	mustStart(t, p)

	alphaPID := lastAlphaPIDFromLog(t, p.AlphaLogPath())
	if alphaPID <= 0 {
		t.Fatal("alpha PID not found in alpha.log")
	}
	alphaNice, err := daemon.ProcessNice(alphaPID)
	if err != nil {
		t.Fatalf("ProcessNice(alpha=%d): %v", alphaPID, err)
	}
	if alphaNice != wantNice {
		t.Errorf("alpha nice = %d, want %d", alphaNice, wantNice)
	}

	svcPID := startedServicePIDFromLog(t, p.AlphaLogPath(), "app")
	svcNice, err := daemon.ProcessNice(svcPID)
	if err != nil {
		t.Fatalf("ProcessNice(service=%d): %v", svcPID, err)
	}
	if svcNice != wantNice {
		t.Errorf("runtime service nice = %d, want %d", svcNice, wantNice)
	}

	stateDir := filepath.Join(p.Dir(), "workspaces", "main")
	if got := readIntFile(t, filepath.Join(stateDir, "build-nice.txt")); got != wantNice {
		t.Errorf("build nice = %d, want %d", got, wantNice)
	}
	if got := readIntFile(t, filepath.Join(stateDir, "provision-nice.txt")); got != wantNice {
		t.Errorf("provision nice = %d, want %d", got, wantNice)
	}

	alphaLog, err := os.ReadFile(p.AlphaLogPath())
	if err != nil {
		t.Fatalf("read alpha log: %v", err)
	}
	wantLogField := fmt.Sprintf("nice=%d", wantNice)
	if !strings.Contains(string(alphaLog), wantLogField) {
		t.Errorf("alpha log missing %q:\n%s", wantLogField, alphaLog)
	}
}

func startedServicePIDFromLog(t *testing.T, logPath, svc string) int {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read %s: %v", logPath, err)
	}
	re := regexp.MustCompile(`started ` + regexp.QuoteMeta(svc) + ` pid=(\d+)`)
	m := re.FindSubmatch(b)
	if len(m) < 2 {
		t.Fatalf("started %s pid=... not found in %s", svc, logPath)
	}
	pid, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parse service pid %q: %v", m[1], err)
	}
	return pid
}

func readIntFile(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse int in %s (%q): %v", path, b, err)
	}
	return n
}
