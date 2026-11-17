package znet

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strconv"
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
	if r.port <= 0 {
		return
	}
	// ReservePort already placed [::1]:port into TIME_WAIT (2*MSL = 30..60s).
	// Skip minting a second TIME_WAIT PCB when HoldPort held the port briefly
	// so rapid bringups do not saturate the kernel's TIME_WAIT table.
	needTimeWait := r.heldAt.IsZero() || time.Since(r.heldAt) >= 5*time.Second
	placed6 := false
	if r.listenFD6 >= 0 {
		if needTimeWait {
			placed6 = transitionToTimeWait6(r.listenFD6, r.port)
		} else {
			_ = syscall.Close(r.listenFD6)
			placed6 = true
		}
		r.listenFD6 = -1
	}
	if r.listenFD >= 0 {
		if needTimeWait && !placed6 {
			_ = transitionToTimeWait4(r.listenFD, r.port)
		} else {
			_ = syscall.Close(r.listenFD)
		}
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
	if r.port <= 0 {
		return
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
		// Lock before probing so a candidate already held by a running service
		// (after its LISTEN holder was released) is skipped without touching its socket.
		if kc.IsSet() {
			u, err := kc.TryLockPort(port)
			if err != nil {
				if errors.Is(err, zlock.ErrLocked) {
					continue
				}
				return nil, err
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
	return nil, errors.New("znet: no free port available in range " + strconv.Itoa(PortRangeBase) + ".." + strconv.Itoa(PortRangeBase+PortRangeSpan-1))
}

func HoldPort(kc zlock.GlobalKeychain, port int) (*Reservation, error) {
	if port <= 0 || port > 65535 {
		panic("znet: HoldPort requires port in 1..65535")
	}
	// Bind and listen before acquiring the flock so the kernel socket is
	// immediately in LISTEN state while lockPortWithRetry waits out any
	// microsecond-lived probe lock from a concurrent ReservePort scan.
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
	// Probe TCPv6 ([::1]) with SO_REUSEADDR=0 FIRST before touching IPv4
	// 127.0.0.1. Because [::1] holds the TIME_WAIT PCB during the handoff
	// window while 127.0.0.1 is left CLOSED for the target service, probing
	// [::1] first ensures a failed probe never momentarily binds 127.0.0.1
	// and collides with the service calling bind(127.0.0.1:port).
	fd6, ok := bindAndListenTCP6Probe(port)
	if !ok {
		return -1, -1, false
	}
	if !probeUDP4Free(port) {
		if fd6 >= 0 {
			_ = syscall.Close(fd6)
		}
		return -1, -1, false
	}
	fd, err := socketCloseOnExec(syscall.AF_INET, syscall.SOCK_STREAM)
	if err != nil {
		if fd6 >= 0 {
			_ = syscall.Close(fd6)
		}
		return -1, -1, false
	}
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		if fd6 >= 0 {
			_ = syscall.Close(fd6)
		}
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
		return -1, isIPv6Unsupported(err)
	}
	sa := &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return -1, isIPv6Unsupported(err)
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

func isIPv6Unsupported(err error) bool {
	return errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EPROTONOSUPPORT) ||
		errors.Is(err, syscall.EADDRNOTAVAIL)
}

func probeUDP4Free(port int) bool {
	// Some services (e.g. CoreDNS) bind the picked port on UDP 127.0.0.1 as well.
	fd, err := socketCloseOnExec(syscall.AF_INET, syscall.SOCK_DGRAM)
	if err != nil {
		return false
	}
	defer func() { _ = syscall.Close(fd) }()
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	return syscall.Bind(fd, sa) == nil
}

func transitionToTimeWait6(listenFD6, port int) bool {
	sa := &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
	return transitionToTimeWait(listenFD6, syscall.AF_INET6, sa)
}

func transitionToTimeWait4(listenFD4, port int) bool {
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	return transitionToTimeWait(listenFD4, syscall.AF_INET, sa)
}

func transitionToTimeWait(listenFD, domain int, sa syscall.Sockaddr) bool {
	curFD := listenFD
	for range 8 {
		done, retry := connectAndClosePeer(curFD, domain, sa)
		if !done {
			if !retry {
				break
			}
			continue
		}
		_ = syscall.Close(curFD)
		curFD = -1
		checkFD, err := socketCloseOnExec(domain, syscall.SOCK_STREAM)
		if err != nil {
			return true
		}
		if err := syscall.Bind(checkFD, sa); err != nil {
			_ = syscall.Close(checkFD)
			return errors.Is(err, syscall.EADDRINUSE)
		}
		if err := syscall.SetsockoptInt(checkFD, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
			_ = syscall.Close(checkFD)
			return false
		}
		if err := syscall.Listen(checkFD, 8); err != nil {
			_ = syscall.Close(checkFD)
			return false
		}
		curFD = checkFD
	}
	if curFD >= 0 {
		_ = syscall.Close(curFD)
	}
	return false
}

func connectAndClosePeer(listenFD, domain int, sa syscall.Sockaddr) (done, retry bool) {
	cliFD, err := socketCloseOnExec(domain, syscall.SOCK_STREAM)
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
	cliPort := localPortOf(cliFD)
	srvFD, err := acceptPeer(listenFD, cliPort)
	if err != nil {
		_ = syscall.Close(cliFD)
		return false, true
	}
	if !closePeerAndWaitEOF(srvFD, cliFD) {
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

func closePeerAndWaitEOF(srvFD, cliFD int) bool {
	_ = syscall.Close(srvFD)
	tv := syscall.Timeval{Usec: 200_000}
	_ = syscall.SetsockoptTimeval(cliFD, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	return readEOF(cliFD)
}

func waitDisconnected(fd int) {
	for range 10000 {
		_, err := syscall.Getpeername(fd)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return
		}
		time.Sleep(50 * time.Microsecond)
	}
}

func localPortOf(fd int) int {
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		return 0
	}
	switch v := sa.(type) {
	case *syscall.SockaddrInet6:
		return v.Port
	case *syscall.SockaddrInet4:
		return v.Port
	default:
		return 0
	}
}

func acceptPeer(listenFD, wantPort int) (int, error) {
	_ = syscall.SetNonblock(listenFD, true)
	defer func() { _ = syscall.SetNonblock(listenFD, false) }()
	for range 4000 {
		fd, rsa, err := acceptCloseOnExec(listenFD)
		if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			time.Sleep(50 * time.Microsecond)
			continue
		}
		if err != nil {
			return -1, err
		}
		if wantPort > 0 && sockaddrPort(rsa) != wantPort {
			_ = syscall.Close(fd)
			continue
		}
		return fd, nil
	}
	return -1, syscall.ETIMEDOUT
}

func sockaddrPort(sa syscall.Sockaddr) int {
	switch v := sa.(type) {
	case *syscall.SockaddrInet6:
		return v.Port
	case *syscall.SockaddrInet4:
		return v.Port
	default:
		return 0
	}
}

func acceptCloseOnExec(listenFD int) (int, syscall.Sockaddr, error) {
	syscall.ForkLock.RLock()
	fd, rsa, err := syscall.Accept(listenFD)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	return fd, rsa, err
}

func readEOF(fd int) bool {
	var buf [1]byte
	for {
		n, err := syscall.Read(fd, buf[:])
		if errors.Is(err, syscall.EINTR) {
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
