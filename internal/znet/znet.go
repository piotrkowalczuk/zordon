package znet

import (
	"crypto/rand"
	"errors"
	"math/big"
	"syscall"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zlock"
)

const (
	// PortRangeBase and PortRangeSpan place dynamically picked ports in
	// 20000..31999, below both Linux's default ephemeral range (32768..60999)
	// and macOS/IANA's (49152..65535) so outbound connections from build tools
	// or probes are not assigned ports from this pool by the kernel.
	PortRangeBase = 20000
	PortRangeSpan = 12000
)

type Reservation struct {
	port      int
	listenFD  int
	listenFD6 int
	heldAt    time.Time
	unlock    zlock.UnlockFunc
}

func (r *Reservation) Port() int {
	if r == nil {
		panic("znet: nil Reservation")
	}
	return r.port
}

func (r *Reservation) TransitionToTimeWait() {
	if r == nil {
		panic("znet: nil Reservation")
	}
	if r.listenFD6 >= 0 {
		if r.heldAt.IsZero() || time.Since(r.heldAt) >= 5*time.Second {
			transitionToTimeWait6(r.listenFD6, r.port)
		} else {
			_ = syscall.Close(r.listenFD6)
		}
		r.listenFD6 = -1
	}
	if r.listenFD >= 0 {
		_ = syscall.Close(r.listenFD)
		r.listenFD = -1
	}
}

func (r *Reservation) ReleaseToTimeWait() {
	if r == nil {
		panic("znet: nil Reservation")
	}
	r.TransitionToTimeWait()
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
}

func (r *Reservation) Close() {
	if r == nil {
		panic("znet: nil Reservation")
	}
	if r.listenFD6 >= 0 {
		_ = syscall.Close(r.listenFD6)
		r.listenFD6 = -1
	}
	if r.listenFD >= 0 {
		_ = syscall.Close(r.listenFD)
		r.listenFD = -1
	}
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
}

func ReservePort(kc zlock.GlobalKeychain, skip map[int]bool) (*Reservation, error) {
	offset := randomOffset(PortRangeSpan)
	for i := range PortRangeSpan {
		port := PortRangeBase + (offset+i)%PortRangeSpan
		if skip[port] {
			continue
		}
		var unlock zlock.UnlockFunc
		if kc.IsSet() {
			u, err := kc.TryLockPort(port)
			if err != nil {
				continue
			}
			unlock = u
		}
		listenFD, listenFD6, ok := bindAndListenProbe(port)
		if !ok {
			if unlock != nil {
				unlock()
			}
			continue
		}
		return &Reservation{
			port:      port,
			listenFD:  listenFD,
			listenFD6: listenFD6,
			unlock:    unlock,
		}, nil
	}
	return nil, errors.New("znet: no free port available in range 20000..31999")
}

func HoldPort(kc zlock.GlobalKeychain, port int) (*Reservation, error) {
	if port <= 0 || port > 65535 {
		panic("znet: HoldPort requires port in 1..65535")
	}
	fd, err := socketCloseOnExec(syscall.AF_INET, syscall.SOCK_STREAM)
	if err != nil {
		return nil, err
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	if err := bindWithRetry(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	if err := syscall.Listen(fd, 8); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	fd6 := holdTCP6(port)
	var unlock zlock.UnlockFunc
	if kc.IsSet() {
		u, lerr := lockPortWithRetry(kc, port)
		if lerr != nil {
			if fd6 >= 0 {
				_ = syscall.Close(fd6)
			}
			_ = syscall.Close(fd)
			return nil, lerr
		}
		unlock = u
	}
	return &Reservation{
		port:      port,
		listenFD:  fd,
		listenFD6: fd6,
		heldAt:    time.Now(),
		unlock:    unlock,
	}, nil
}

func holdTCP6(port int) int {
	fd6, err := socketCloseOnExec(syscall.AF_INET6, syscall.SOCK_STREAM)
	if err != nil {
		return -1
	}
	_ = syscall.SetsockoptInt(fd6, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1)
	if err := syscall.SetsockoptInt(fd6, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		_ = syscall.Close(fd6)
		return -1
	}
	sa6 := &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
	if err := bindWithRetry(fd6, sa6); err != nil {
		_ = syscall.Close(fd6)
		return -1
	}
	if err := syscall.Listen(fd6, 8); err != nil {
		_ = syscall.Close(fd6)
		return -1
	}
	return fd6
}

func bindWithRetry(fd int, sa syscall.Sockaddr) error {
	var lastErr error
	for range 100 {
		err := syscall.Bind(fd, sa)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, syscall.EADDRINUSE) {
			return err
		}
		time.Sleep(100 * time.Microsecond)
	}
	return lastErr
}

func lockPortWithRetry(kc zlock.GlobalKeychain, port int) (zlock.UnlockFunc, error) {
	var lastErr error
	for range 200 {
		unlock, err := kc.TryLockPort(port)
		if err == nil {
			return unlock, nil
		}
		lastErr = err
		if !errors.Is(err, zlock.ErrLocked) {
			return nil, err
		}
		time.Sleep(250 * time.Microsecond)
	}
	return nil, lastErr
}

func bindAndListenProbe(port int) (int, int, bool) {
	// Bind TCPv4 and TCPv6 ([::1]) with SO_REUSEADDR=0 first so the kernel
	// rejects ports in TIME_WAIT as well as LISTEN/ESTABLISHED. Transitioning
	// the IPv6 loopback listener into TIME_WAIT keeps SO_REUSEADDR=0 probes out
	// while leaving IPv4 127.0.0.1 free of TIME_WAIT for runtimes (such as
	// JDK's HttpServer/ServerSocket) that bind 127.0.0.1 with SO_REUSEADDR=0.
	fd, err := socketCloseOnExec(syscall.AF_INET, syscall.SOCK_STREAM)
	if err != nil {
		return -1, -1, false
	}
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return -1, -1, false
	}
	if !probeUDP4Free(port) {
		_ = syscall.Close(fd)
		return -1, -1, false
	}
	fd6, ok := bindAndListenTCP6Probe(port)
	if !ok {
		_ = syscall.Close(fd)
		return -1, -1, false
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		if fd6 >= 0 {
			_ = syscall.Close(fd6)
		}
		_ = syscall.Close(fd)
		return -1, -1, false
	}
	if err := syscall.Listen(fd, 8); err != nil {
		if fd6 >= 0 {
			_ = syscall.Close(fd6)
		}
		_ = syscall.Close(fd)
		return -1, -1, false
	}
	return fd, fd6, true
}

func bindAndListenTCP6Probe(port int) (int, bool) {
	fd, err := socketCloseOnExec(syscall.AF_INET6, syscall.SOCK_STREAM)
	if err != nil {
		return -1, true
	}
	_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1)
	sa := &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		if errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL) {
			return -1, true
		}
		return -1, false
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		_ = syscall.Close(fd)
		return -1, false
	}
	if err := syscall.Listen(fd, 8); err != nil {
		_ = syscall.Close(fd)
		return -1, false
	}
	return fd, true
}

func probeUDP4Free(port int) bool {
	fd, err := socketCloseOnExec(syscall.AF_INET, syscall.SOCK_DGRAM)
	if err != nil {
		return false
	}
	defer func() { _ = syscall.Close(fd) }()
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	return syscall.Bind(fd, sa) == nil
}

func transitionToTimeWait6(listenFD6, port int) {
	sa := &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
	for range 8 {
		done, retry := connectAndClosePeer6(listenFD6, sa)
		if done || !retry {
			break
		}
	}
	_ = syscall.Close(listenFD6)
}

func connectAndClosePeer6(listenFD6 int, sa *syscall.SockaddrInet6) (done, retry bool) {
	cliFD, err := socketCloseOnExec(syscall.AF_INET6, syscall.SOCK_STREAM)
	if err != nil {
		return false, false
	}
	connErr := syscall.Connect(cliFD, sa)
	if connErr != nil && !errors.Is(connErr, syscall.EINTR) {
		_ = syscall.Close(cliFD)
		if errors.Is(connErr, syscall.EPERM) || errors.Is(connErr, syscall.EACCES) {
			return false, false
		}
		return false, true
	}
	cliPort := localPortOf6(cliFD)
	srvFD, err := acceptPeer6(listenFD6, cliPort)
	if err != nil {
		_ = syscall.Close(cliFD)
		return false, true
	}
	syscall.CloseOnExec(srvFD)
	_ = syscall.Close(srvFD)
	if !readEOF(cliFD) {
		_ = syscall.Close(cliFD)
		return false, true
	}
	if err := syscall.Shutdown(cliFD, syscall.SHUT_WR); err != nil {
		_ = syscall.Close(cliFD)
		return false, true
	}
	waitDisconnected(cliFD)
	_ = syscall.Close(cliFD)
	return true, false
}

func waitDisconnected(fd int) {
	for range 10000 {
		if _, err := syscall.Getpeername(fd); err != nil {
			return
		}
		time.Sleep(50 * time.Microsecond)
	}
}

func localPortOf6(fd int) int {
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		return 0
	}
	if inet6, ok := sa.(*syscall.SockaddrInet6); ok {
		return inet6.Port
	}
	return 0
}

func acceptPeer6(listenFD, wantPort int) (int, error) {
	for {
		fd, rsa, err := syscall.Accept(listenFD)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return -1, err
		}
		if wantPort > 0 {
			if inet6, ok := rsa.(*syscall.SockaddrInet6); ok && inet6.Port != wantPort {
				syscall.CloseOnExec(fd)
				_ = syscall.Close(fd)
				continue
			}
		}
		return fd, nil
	}
}

func readEOF(fd int) bool {
	var buf [1]byte
	for {
		n, err := syscall.Read(fd, buf[:])
		if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) {
			continue
		}
		return n == 0 && err == nil
	}
}

func socketCloseOnExec(domain, typ int) (int, error) {
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(domain, typ, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	return fd, err
}

func randomOffset(span int) int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(span)))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}
