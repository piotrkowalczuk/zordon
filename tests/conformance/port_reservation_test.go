//go:build conformance_go

package conformance_test

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zlock"
	"github.com/piotrkowalczuk/zordon/internal/znet"
	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestPortReservation_heldDuringBuildAndLockedAtRuntime(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/app")
	p.WriteFile("src/app/port_probe.go", `//go:build ignore

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

func main() {
	port, err := strconv.Atoi(os.Args[1])
	if err != nil {
		panic(err)
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		panic(err)
	}
	defer func() { _ = syscall.Close(fd) }()
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	held := syscall.Bind(fd, sa) != nil
	fmt.Printf("PORT=%d\nHELD=%t\n", port, held)
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

  vars = { port = net::pickport() }

  build {
    cmd = ["/bin/sh", "-c", "go run ./port_probe.go ${self.vars.port} > \"${fs::state()}/build-port.txt\"; go build -o \"${fs::bin()}/app\" ."]
  }

  runtime {
    cmd = ["${fs::bin()}/app", "-addr", "127.0.0.1:${self.vars.port}"]
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

	stateDir := filepath.Join(p.Dir(), "workspaces", "main")
	report := readBuildProbeReport(t, filepath.Join(stateDir, "build-port.txt"))
	if report["HELD"] != "true" {
		t.Fatalf("build probe HELD = %q, want \"true\" (full=%v)", report["HELD"], report)
	}
	port, err := strconv.Atoi(report["PORT"])
	if err != nil {
		t.Fatalf("parse PORT %q: %v", report["PORT"], err)
	}
	if port < znet.PortRangeBase || port >= znet.PortRangeBase+znet.PortRangeSpan {
		t.Fatalf("picked port %d outside [%d, %d)", port, znet.PortRangeBase, znet.PortRangeBase+znet.PortRangeSpan)
	}

	kc := zlock.NewGlobalKeychain(p.Home())
	if _, err := kc.TryLockPort(port); !errors.Is(err, zlock.ErrLocked) {
		t.Fatalf("TryLockPort(%d) while service running = %v, want ErrLocked", port, err)
	}

	stopRes := p.Zordon("stop").Run(t)
	if stopRes.ExitCode != 0 {
		t.Fatalf("zordon stop exit=%d: %s", stopRes.ExitCode, stopRes.Stderr)
	}
	if err := kc.ForWorkspace(stateDir).WaitUnlockedAlpha(t.Context()); err != nil {
		t.Fatalf("WaitUnlockedAlpha after zordon stop: %v", err)
	}

	unlock, err := kc.TryLockPort(port)
	if err != nil {
		t.Fatalf("TryLockPort(%d) after zordon stop: %v", port, err)
	}
	unlock()
}
