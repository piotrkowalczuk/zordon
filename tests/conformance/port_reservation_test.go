//go:build conformance_go

package conformance_test

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlock"
	"github.com/piotrkowalczuk/zordon/internal/znet"
	"github.com/piotrkowalczuk/zordon/internal/zordontest"
	"github.com/piotrkowalczuk/zordon/internal/ztest"
)

func TestStart_portReservationHeldDuringBuildAndLockedAtRuntime(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/app")
	p.WriteFile("src/app/port_probe.go", `//go:build ignore

package main

import (
	"fmt"
	"os"
	"path/filepath"
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

	lockPath := filepath.Join(os.Args[2], "locks", fmt.Sprintf("port-%d.lock", port))
	locked := false
	if lf, err := os.OpenFile(lockPath, os.O_RDWR, 0); err == nil {
		if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			locked = true
		} else {
			_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		}
		_ = lf.Close()
	}
	fmt.Printf("PORT=%d\nHELD=%t\nLOCKED=%t\n", port, held, locked)
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
    cmd = ["/bin/sh", "-c", "go run ./port_probe.go ${self.vars.port} '`+p.Home()+`' > \"${fs::state()}/build-port.txt\"; go build -o \"${fs::bin()}/app\" ."]
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
	if report["LOCKED"] != "true" {
		t.Fatalf("build probe LOCKED = %q, want \"true\" (full=%v)", report["LOCKED"], report)
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

func TestStart_multipleWorkspacesPickDistinctLockedPorts(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/app")
	p.GitInit("src/app")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "app" {
  src {
    path = "./src/app"
    exe  = "."
  }

  vars = { port = net::pickport() }

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

	mustZordonOK(t, p, "workspace", "create", "wt1", "app")
	wtRel := filepath.Join("workspaces", "wt1")
	t.Cleanup(func() {
		_ = p.Zordon("stop").WithDir(wtRel).Run(t)
		_ = p.Zordon("stop").Run(t)
	})

	resMain := p.Zordon("start").WithTimeout(2 * time.Minute).Run(t)
	if resMain.ExitCode != 0 {
		t.Fatalf("main zordon start exit=%d: %s\n%s", resMain.ExitCode, resMain.Stdout, resMain.Stderr)
	}
	resWT1 := p.Zordon("start").WithDir(wtRel).WithTimeout(2 * time.Minute).Run(t)
	if resWT1.ExitCode != 0 {
		t.Fatalf("wt1 zordon start exit=%d: %s\n%s", resWT1.ExitCode, resWT1.Stdout, resWT1.Stderr)
	}

	portMain := p.Get(t, "service.go.app.vars.port").Int()
	portWT1 := p.GetIn(t, wtRel, "service.go.app.vars.port").Int()
	if portMain == portWT1 {
		t.Fatalf("workspace starts picked same port %d", portMain)
	}

	kc := zlock.NewGlobalKeychain(p.Home())
	for _, port := range []int{portMain, portWT1} {
		if _, err := kc.TryLockPort(port); !errors.Is(err, zlock.ErrLocked) {
			t.Fatalf("TryLockPort(%d) while service running = %v, want ErrLocked", port, err)
		}
	}
}

func TestReservePort_readOnlyCommandsDoNotCreatePortLocksOrTimeWait(t *testing.T) {
	p := zordontest.NewProject(t, zordontest.WithIsolatedHome(t.TempDir()))
	p.CopyTree("golden/go/echo", "src/app")
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "a" {
  src {
    path = "./src/app"
    exe  = "."
  }
  vars = { port = net::pickport() }
}

service "go" "b" {
  src {
    path = "./src/app"
    exe  = "."
  }
  vars = { port = net::pickport() }
}
`)

	for range 5 {
		mustZordonOK(t, p, "status")
	}
	mustZordonOK(t, p, "plan")
	port := p.Get(t, "service.go.a.vars.port").Int()

	entries, err := zfs.ReadDir(filepath.Join(p.Home(), "locks"))
	if err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "port-") {
				t.Fatalf("read-only commands created unexpected port lockfile %s in %s/locks", e.Name(), p.Home())
			}
		}
	}

	fd4, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socket(AF_INET): %v", err)
	}
	defer func() { _ = syscall.Close(fd4) }()
	if err := syscall.Bind(fd4, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("Bind(127.0.0.1:%d) with SO_REUSEADDR=0 failed after zordon get: %v", port, err)
	}

	fd6, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0)
	if err == nil {
		defer func() { _ = syscall.Close(fd6) }()
		if err := syscall.Bind(fd6, &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}); err != nil && !errors.Is(err, syscall.EAFNOSUPPORT) && !errors.Is(err, syscall.EADDRNOTAVAIL) {
			t.Fatalf("Bind([::1]:%d) with SO_REUSEADDR=0 failed after zordon get (TIME_WAIT leaked): %v", port, err)
		}
	}
}
