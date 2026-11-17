package main

import (
	"errors"
	"net"
	"strconv"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zlock"
	"github.com/piotrkowalczuk/zordon/internal/znet"
)

func TestServiceCtx_holdAndReleasePorts(t *testing.T) {
	kc := zlock.NewGlobalKeychain(t.TempDir())
	res, err := znet.ReservePort(kc, nil)
	if err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	port := res.Port()
	res.ReleaseToTimeWait()

	sc := newServiceCtx("api", "go")
	sc.holdPorts([]int{port}, kc, nil)
	defer sc.releasePorts()

	if _, err := kc.TryLockPort(port); !errors.Is(err, zlock.ErrLocked) {
		t.Fatalf("TryLockPort(%d) while held = %v, want ErrLocked", port, err)
	}

	sc.releasePortHolders()

	if _, err := kc.TryLockPort(port); !errors.Is(err, zlock.ErrLocked) {
		t.Fatalf("TryLockPort(%d) after releasePortHolders = %v, want ErrLocked (flock stays held while service runs)", port, err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("service Listen(%d) after releasePortHolders: %v", port, err)
	}
	_ = ln.Close()

	sc.releasePorts()

	unlock, err := kc.TryLockPort(port)
	if err != nil {
		t.Fatalf("TryLockPort(%d) after releasePorts: %v", port, err)
	}
	unlock()
}
