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
	// 20000..31999, below Linux's default ephemeral range (32768..60999) and
	// macOS/IANA's (49152..65535) so outbound connections from build tools or
	// probes are not assigned ports from this pool under default sysctls.
	PortRangeBase = 20000
	PortRangeSpan = 12000
)

type Reservation struct {
	port   int
	tcpFD  int
	tcpFD6 int
	udpFD  int
	udpFD6 int
	heldAt time.Time
	unlock zlock.UnlockFunc
}

func (r *Reservation) Port() int {
	if r == nil {
		panic("znet: nil Reservation")
	}
	return r.port
}

// TransitionToTimeWait closes the held loopback sockets while keeping the
// cross-process flock (if any) held. To guard the handoff window against
// processes that do not share ZORDON_HOME (and therefore do not see the flock),
// the IPv6 loopback socket ([::1]:port) is transitioned into TIME_WAIT while
// IPv4 (127.0.0.1:port) is closed directly from TCP_CLOSE to CLOSED so IPv4
// services that bind 127.0.0.1 or 0.0.0.0 without SO_REUSEADDR never collide
// with TIME_WAIT. Services binding IPv6 ([::1] or [::]) directly must use
// SO_REUSEADDR (the default in Go, Rust, Node.js, and Ruby). Returns true when
// the [::1] TIME_WAIT guard is active.
func (r *Reservation) TransitionToTimeWait() bool {
	if r == nil {
		panic("znet: nil Reservation")
	}
	if r.port <= 0 {
		return false
	}
	placed := false
	if r.tcpFD6 >= 0 {
		// ReservePort already placed [::1]:port into TIME_WAIT (2*MSL = 30..60s).
		// Skip minting a second TIME_WAIT PCB when HoldPort held the port briefly
		// so rapid bringups do not saturate the kernel's TIME_WAIT table.
		if r.heldAt.IsZero() || time.Since(r.heldAt) >= 5*time.Second {
			placed = transitionToTimeWait6(r.tcpFD6, r.port)
		} else {
			_ = syscall.Close(r.tcpFD6)
			placed = true
		}
		r.tcpFD6 = -1
	}
	if r.tcpFD >= 0 {
		_ = syscall.Close(r.tcpFD)
		r.tcpFD = -1
	}
	if r.udpFD6 >= 0 {
		_ = syscall.Close(r.udpFD6)
		r.udpFD6 = -1
	}
	if r.udpFD >= 0 {
		_ = syscall.Close(r.udpFD)
		r.udpFD = -1
	}
	return placed
}

func (r *Reservation) ReleaseToTimeWait() bool {
	if r == nil {
		panic("znet: nil Reservation")
	}
	placed := r.TransitionToTimeWait()
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
	return placed
}

func (r *Reservation) Close() {
	if r == nil {
		panic("znet: nil Reservation")
	}
	if r.port <= 0 {
		return
	}
	closeIfValid(r.tcpFD6)
	r.tcpFD6 = -1
	closeIfValid(r.tcpFD)
	r.tcpFD = -1
	closeIfValid(r.udpFD6)
	r.udpFD6 = -1
	closeIfValid(r.udpFD)
	r.udpFD = -1
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
		// (after its socket holder was released) is skipped without touching it.
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
		tcpFD, tcpFD6, udpFD, udpFD6, ok := bindLoopbackProbe(port)
		if !ok {
			if unlock != nil {
				unlock()
			}
			continue
		}
		return &Reservation{
			port:   port,
			tcpFD:  tcpFD,
			tcpFD6: tcpFD6,
			udpFD:  udpFD,
			udpFD6: udpFD6,
			unlock: unlock,
		}, nil
	}
	return nil, errors.New("znet: no free port available in range " + strconv.Itoa(PortRangeBase) + ".." + strconv.Itoa(PortRangeBase+PortRangeSpan-1))
}

func HoldPort(kc zlock.GlobalKeychain, port int) (*Reservation, error) {
	if port <= 0 || port > 65535 {
		panic("znet: HoldPort requires port in 1..65535")
	}
	// Bind loopback sockets (127.0.0.1 and [::1], avoiding 0.0.0.0 which
	// triggers the macOS application firewall prompt) without calling Listen
	// before acquiring the flock. Keeping the sockets bound in TCP_CLOSE state
	// blocks 127.0.0.1 and [::1] binds on Darwin and Linux while immediately
	// returning ECONNREFUSED to probing dialers instead of queuing them in an
	// unserved accept backlog.
	tcpFD, err := socketCloseOnExec(syscall.AF_INET, syscall.SOCK_STREAM)
	if err != nil {
		return nil, err
	}
	sa4 := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	if err := bindWithRetry(tcpFD, sa4); err != nil {
		_ = syscall.Close(tcpFD)
		return nil, err
	}
	tcpFD6 := holdTCP6(port)
	udpFD := holdUDP4(port)
	udpFD6 := holdUDP6(port)
	var unlock zlock.UnlockFunc
	if kc.IsSet() {
		u, lerr := lockPortWithRetry(kc, port)
		if lerr != nil {
			closeIfValid(udpFD6)
			closeIfValid(udpFD)
			closeIfValid(tcpFD6)
			_ = syscall.Close(tcpFD)
			return nil, lerr
		}
		unlock = u
	}
	return &Reservation{
		port:   port,
		tcpFD:  tcpFD,
		tcpFD6: tcpFD6,
		udpFD:  udpFD,
		udpFD6: udpFD6,
		heldAt: time.Now(),
		unlock: unlock,
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
	_ = syscall.SetsockoptInt(fd6, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
	return fd6
}

func holdUDP4(port int) int {
	fd, err := socketCloseOnExec(syscall.AF_INET, syscall.SOCK_DGRAM)
	if err != nil {
		return -1
	}
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	if err := bindWithRetry(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return -1
	}
	return fd
}

func holdUDP6(port int) int {
	fd6, err := socketCloseOnExec(syscall.AF_INET6, syscall.SOCK_DGRAM)
	if err != nil {
		return -1
	}
	sa6 := &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
	if err := bindWithRetry(fd6, sa6); err != nil {
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

func bindLoopbackProbe(port int) (int, int, int, int, bool) {
	// Probe TCPv6 ([::1]) with SO_REUSEADDR=0 FIRST before touching IPv4
	// 127.0.0.1. Because [::1] holds the TIME_WAIT PCB during the handoff
	// window while 127.0.0.1 is left CLOSED for the target service, probing
	// [::1] first ensures a failed probe never momentarily binds 127.0.0.1
	// and collides with the service calling bind(127.0.0.1:port).
	tcpFD6, ok := bindLoopback6Probe(port, syscall.SOCK_STREAM)
	if !ok {
		return -1, -1, -1, -1, false
	}
	udpFD6, ok := bindLoopback6Probe(port, syscall.SOCK_DGRAM)
	if !ok {
		closeIfValid(tcpFD6)
		return -1, -1, -1, -1, false
	}
	udpFD, ok := bindLoopback4Probe(port, syscall.SOCK_DGRAM)
	if !ok {
		closeIfValid(udpFD6)
		closeIfValid(tcpFD6)
		return -1, -1, -1, -1, false
	}
	tcpFD, ok := bindLoopback4Probe(port, syscall.SOCK_STREAM)
	if !ok {
		closeIfValid(udpFD)
		closeIfValid(udpFD6)
		closeIfValid(tcpFD6)
		return -1, -1, -1, -1, false
	}
	return tcpFD, tcpFD6, udpFD, udpFD6, true
}

func bindLoopback4Probe(port, typ int) (int, bool) {
	fd, err := socketCloseOnExec(syscall.AF_INET, typ)
	if err != nil {
		return -1, false
	}
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return -1, false
	}
	return fd, true
}

func bindLoopback6Probe(port, typ int) (int, bool) {
	fd, err := socketCloseOnExec(syscall.AF_INET6, typ)
	if err != nil {
		return -1, isIPv6Unsupported(err)
	}
	sa := &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return -1, isIPv6Unsupported(err)
	}
	return fd, true
}

func isIPv6Unsupported(err error) bool {
	return errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EPROTONOSUPPORT) ||
		errors.Is(err, syscall.EADDRNOTAVAIL)
}

func transitionToTimeWait6(tcpFD6, port int) bool {
	sa := &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
	if err := syscall.SetsockoptInt(tcpFD6, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		_ = syscall.Close(tcpFD6)
		return false
	}
	if err := syscall.Listen(tcpFD6, 8); err != nil {
		_ = syscall.Close(tcpFD6)
		return false
	}
	curFD := tcpFD6
	for range 8 {
		done, retry := connectAndClosePeer6(curFD, sa)
		if !done {
			if !retry {
				break
			}
			continue
		}
		_ = syscall.Close(curFD)
		curFD = -1
		checkFD, err := socketCloseOnExec(syscall.AF_INET6, syscall.SOCK_STREAM)
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
	closeIfValid(curFD)
	return false
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
		if wantPort > 0 {
			if inet6, ok := rsa.(*syscall.SockaddrInet6); ok && inet6.Port != wantPort {
				_ = syscall.Close(fd)
				continue
			}
		}
		return fd, nil
	}
	return -1, syscall.ETIMEDOUT
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

func closeIfValid(fd int) {
	if fd >= 0 {
		_ = syscall.Close(fd)
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
