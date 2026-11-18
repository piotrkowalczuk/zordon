package znet

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zlock"
	"github.com/piotrkowalczuk/zordon/internal/ztest"
)

func TestReservePort_rangeAndUniqueness(t *testing.T) {
	kc := zlock.NewGlobalKeychain(t.TempDir())
	seen := make(map[int]bool)
	reservations := make([]*Reservation, 0, 16)
	defer func() {
		for _, r := range reservations {
			r.Close()
		}
	}()

	for range 16 {
		res, err := ReservePort(kc, seen)
		if err != nil {
			t.Fatalf("ReservePort: %v", err)
		}
		reservations = append(reservations, res)
		p := res.Port()
		if p < PortRangeBase || p >= PortRangeBase+PortRangeSpan {
			t.Fatalf("Port() = %d, want in [%d, %d)", p, PortRangeBase, PortRangeBase+PortRangeSpan)
		}
		if seen[p] {
			t.Fatalf("duplicate port %d", p)
		}
		seen[p] = true
	}
}

func TestReservePort_skipsFlockAndSkipMap(t *testing.T) {
	kc := zlock.NewGlobalKeychain(t.TempDir())

	first, err := ReservePort(kc, nil)
	if err != nil {
		t.Fatalf("ReservePort first: %v", err)
	}
	targetPort := first.Port()
	first.Close()

	skipAllExceptTwo := make(map[int]bool, PortRangeSpan)
	otherPort := PortRangeBase + (targetPort-PortRangeBase+1)%PortRangeSpan
	for i := range PortRangeSpan {
		p := PortRangeBase + i
		if p != targetPort && p != otherPort {
			skipAllExceptTwo[p] = true
		}
	}

	unlock, err := kc.TryLockPort(targetPort)
	if err != nil {
		t.Fatalf("TryLockPort(%d): %v", targetPort, err)
	}
	defer unlock()

	second, err := ReservePort(kc, skipAllExceptTwo)
	if err != nil {
		t.Fatalf("ReservePort second: %v", err)
	}
	defer second.Close()

	if second.Port() != otherPort {
		t.Fatalf("ReservePort picked %d, want %d (since %d is flock-locked)", second.Port(), otherPort, targetPort)
	}

	skipAllExceptLocked := make(map[int]bool, PortRangeSpan)
	for i := range PortRangeSpan {
		p := PortRangeBase + i
		if p != targetPort {
			skipAllExceptLocked[p] = true
		}
	}
	if _, err := ReservePort(kc, skipAllExceptLocked); err == nil {
		t.Fatal("ReservePort succeeded when only non-skipped port is flock-locked; want error")
	}
}

func TestReservePort_timeWaitAndHoldPort(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	kc := zlock.NewGlobalKeychain(t.TempDir())

	res, err := ReservePort(kc, nil)
	if err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	port := res.Port()

	if tryBindLoopbackProbe(port) {
		res.Close()
		t.Fatalf("bindLoopbackProbe(%d) succeeded while Reservation is active", port)
	}

	if !res.TransitionToTimeWait() {
		res.Close()
		t.Fatalf("TransitionToTimeWait(%d) = false, want true", port)
	}
	res.Close()

	if tryBindLoopbackProbe(port) {
		t.Fatalf("bindLoopbackProbe(%d) succeeded while port is in TIME_WAIT", port)
	}

	holder, err := HoldPort(kc, port)
	if err != nil {
		t.Fatalf("HoldPort(%d) over TIME_WAIT: %v", port, err)
	}
	defer holder.Close()

	if tryBindLoopbackProbe(port) {
		t.Fatalf("bindLoopbackProbe(%d) succeeded while HoldPort is active", port)
	}

	if !holder.TransitionToTimeWait() {
		t.Fatalf("holder.TransitionToTimeWait(%d) = false, want true", port)
	}

	if tryBindLoopbackProbe(port) {
		t.Fatalf("bindLoopbackProbe(%d) succeeded after HoldPort TransitionToTimeWait", port)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("service net.Listen on %d after HoldPort TransitionToTimeWait: %v", port, err)
	}
	_ = ln.Close()
}

func TestReservePort_raceStress60s(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	const (
		stressDuration = 60 * time.Second
		burstInterval  = 200 * time.Millisecond
		workers        = 8
		windowSize     = 16
		stealers       = 4
	)

	kc := zlock.NewGlobalKeychain(t.TempDir())
	var (
		activeOwners [65536]atomic.Int32
		currentBase  atomic.Int32
		totalCycles  atomic.Int64
		stopStealers atomic.Bool
		stealerWG    sync.WaitGroup
	)
	coprimeStrides := [...]int{17, 29, 43, 61, 89, 113, 149, 197, 251, 313, 389, 463}
	stride := coprimeStrides[randomOffset(len(coprimeStrides))]
	startOffset := randomOffset(PortRangeSpan)
	currentBase.Store(int32(startOffset))

	for s := range stealers {
		stealerWG.Add(1)
		go func(id int) {
			defer stealerWG.Done()
			for !stopStealers.Load() {
				winOffset := int(currentBase.Load())
				for i := range windowSize {
					p := PortRangeBase + (winOffset+i*stride)%PortRangeSpan
					phaseBefore := activeOwners[p].Load()
					if phaseBefore == 0 {
						continue
					}
					stolen := false
					if id == 0 {
						stolen = tryBindLoopbackProbe(p)
					} else {
						stolen = tryStealReservedPort(kc, p)
					}
					if stolen {
						if phaseAfter := activeOwners[p].Load(); phaseAfter != 0 {
							t.Errorf("RACE CONDITION: stealer %d bound reserved port %d while owned by worker (phaseBefore=%d phaseAfter=%d)", id, p, phaseBefore, phaseAfter)
							return
						}
					}
				}
				time.Sleep(time.Millisecond)
			}
		}(s)
	}
	defer func() {
		stopStealers.Store(true)
		stealerWG.Wait()
	}()

	deadline := time.Now().Add(stressDuration)
	for burst := 0; time.Now().Before(deadline); burst++ {
		burstStart := time.Now()
		winOffset := (startOffset + burst*windowSize*stride) % PortRangeSpan
		currentBase.Store(int32(winOffset))
		skip := buildSkipOutsideWindow(winOffset, windowSize, stride)

		startBurst := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(workers)
		for range workers {
			go func() {
				defer wg.Done()
				<-startBurst
				runStressWorkerCycle(t, kc, skip, &activeOwners, &totalCycles)
			}()
		}
		close(startBurst)
		wg.Wait()

		if t.Failed() {
			break
		}
		if elapsed := time.Since(burstStart); elapsed < burstInterval {
			time.Sleep(burstInterval - elapsed)
		}
	}

	t.Logf("completed %d concurrent reserve->hold->bind->release lifecycles over %s without collision", totalCycles.Load(), stressDuration)
	if totalCycles.Load() == 0 {
		t.Fatal("expected > 0 completed cycles")
	}
}

func TestReservation_panicsOnInvalidUse(t *testing.T) {
	assertPanics(t, "nil.Port", func() {
		var r *Reservation
		_ = r.Port()
	})
	assertPanics(t, "nil.TransitionToTimeWait", func() {
		var r *Reservation
		_ = r.TransitionToTimeWait()
	})
	assertPanics(t, "nil.Close", func() {
		var r *Reservation
		r.Close()
	})
	assertPanics(t, "HoldPort(0)", func() {
		_, _ = HoldPort(zlock.GlobalKeychain{}, 0)
	})
	assertPanics(t, "HoldPort(70000)", func() {
		_, _ = HoldPort(zlock.GlobalKeychain{}, 70000)
	})
}

func TestReservation_Close_zeroValueDoesNotCloseStdin(t *testing.T) {
	saved0, err := syscall.Dup(0)
	if err != nil {
		t.Fatalf("Dup(0): %v", err)
	}
	defer func() {
		var st syscall.Stat_t
		if syscall.Fstat(0, &st) != nil {
			_, _ = syscall.Dup(saved0)
		}
		_ = syscall.Close(saved0)
	}()

	var zero Reservation
	if zero.TransitionToTimeWait() {
		t.Fatal("zero.TransitionToTimeWait() = true, want false")
	}
	zero.Close()

	var st syscall.Stat_t
	if err := syscall.Fstat(0, &st); err != nil {
		t.Fatalf("zero Reservation closed fd 0 (stdin): Fstat(0) = %v", err)
	}
}

func TestReservePort_propagatesLockIOError(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "locks"), []byte("not-a-dir"), 0o600); err != nil {
		t.Fatalf("write locks file: %v", err)
	}
	kc := zlock.NewGlobalKeychain(home)

	_, err := ReservePort(kc, nil)
	if err == nil {
		t.Fatal("ReservePort succeeded with broken locks path; want I/O error")
	}
	if strings.Contains(err.Error(), "no free port available") {
		t.Fatalf("ReservePort swallowed lock I/O error and returned %q; want underlying filesystem error", err.Error())
	}
}

func TestClosePeerAndWaitEOF_leakedServerFDDoesNotHang(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	cliFD, srvFD := fds[0], fds[1]
	defer func() {
		_ = syscall.Close(cliFD)
		_ = syscall.Close(srvFD)
	}()

	leakedChildFD, err := syscall.Dup(srvFD)
	if err != nil {
		t.Fatalf("Dup(srvFD): %v", err)
	}
	defer func() { _ = syscall.Close(leakedChildFD) }()

	done := make(chan bool, 1)
	go func() {
		done <- closePeerAndWaitEOF(srvFD, cliFD)
	}()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("closePeerAndWaitEOF returned true when peer fd leaked and never sent FIN; want false")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("closePeerAndWaitEOF hung forever when peer fd leaked (SO_RCVTIMEO EAGAIN was ignored)")
	}
}

func TestReservePort_probeDoesNotTouchIPv4WhileIPv6InTimeWait(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	kc := zlock.NewGlobalKeychain(t.TempDir())

	res, err := ReservePort(kc, nil)
	if err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	port := res.Port()
	_ = res.TransitionToTimeWait()
	res.Close()

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(4)
	for range 4 {
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if tryBindLoopbackProbe(port) {
					t.Errorf("tryBindLoopbackProbe(%d) succeeded while [::1]:%d is in TIME_WAIT", port, port)
					return
				}
			}
		}()
	}
	defer func() {
		stop.Store(true)
		wg.Wait()
	}()

	var lc net.ListenConfig
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for i := range 200 {
		ln, err := lc.Listen(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatalf("iteration %d: service Listen(%s) failed while concurrent probes ran: %v", i, addr, err)
		}
		_ = ln.Close()
	}
}

func TestBindLoopback6Probe_rejectsSocketEMFILE(t *testing.T) {
	// Temporarily set RLIMIT_NOFILE soft limit to 0 (no t.Parallel and no
	// child processes in this test) to verify transient fd exhaustion is not
	// mistaken for missing kernel IPv6 support.
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
		t.Fatalf("Getrlimit(RLIMIT_NOFILE): %v", err)
	}
	zero := syscall.Rlimit{Cur: 0, Max: orig.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &zero); err != nil {
		t.Fatalf("Setrlimit(RLIMIT_NOFILE, 0): %v", err)
	}
	defer func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &orig)
	}()

	fd6, ok := bindLoopback6Probe(PortRangeBase, syscall.SOCK_STREAM)
	if fd6 >= 0 {
		_ = syscall.Close(fd6)
	}
	if ok {
		t.Fatal("bindLoopback6Probe returned ok=true on EMFILE; want ok=false so transient fd exhaustion does not bypass IPv6 probe")
	}
}

func TestHoldPort_blocksTCPAndUDPBindsAndRefusesDialers(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	kc := zlock.NewGlobalKeychain(t.TempDir())

	res, err := ReservePort(kc, nil)
	if err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	port := res.Port()
	_ = res.TransitionToTimeWait()
	res.Close()

	holder, err := HoldPort(kc, port)
	if err != nil {
		t.Fatalf("HoldPort(%d): %v", port, err)
	}
	defer holder.Close()

	addr4 := "127.0.0.1:" + strconv.Itoa(port)
	var lc net.ListenConfig
	if ln, err := lc.Listen(t.Context(), "tcp4", addr4); err == nil {
		_ = ln.Close()
		t.Fatalf("tcp4 Listen(%s) succeeded while HoldPort is active; want EADDRINUSE", addr4)
	}
	if pc, err := lc.ListenPacket(t.Context(), "udp4", addr4); err == nil {
		_ = pc.Close()
		t.Fatalf("udp4 ListenPacket(%s) succeeded while HoldPort is active; want EADDRINUSE", addr4)
	}

	var d net.Dialer
	d.Timeout = 200 * time.Millisecond
	if c, err := d.DialContext(t.Context(), "tcp4", addr4); err == nil {
		_ = c.Close()
		t.Fatalf("tcp4 Dial(%s) succeeded while HoldPort is active; want ECONNREFUSED so probing clients do not hang", addr4)
	}

	_ = holder.TransitionToTimeWait()

	pc, err := lc.ListenPacket(t.Context(), "udp4", addr4)
	if err != nil {
		t.Fatalf("udp4 ListenPacket(%s) after TransitionToTimeWait: %v", addr4, err)
	}
	_ = pc.Close()
}

func TestHoldPort_allowsRawZeroReuseAddrIPv4BindWhenIPv6Unavailable(t *testing.T) {
	kc := zlock.NewGlobalKeychain(t.TempDir())

	res, err := ReservePort(kc, nil)
	if err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	port := res.Port()
	if res.tcpFD6 >= 0 {
		_ = syscall.Close(res.tcpFD6)
		res.tcpFD6 = -1
	}
	if res.TransitionToTimeWait() {
		res.Close()
		t.Fatal("TransitionToTimeWait() = true when tcpFD6 == -1, want false")
	}
	res.Close()

	holder, err := HoldPort(kc, port)
	if err != nil {
		t.Fatalf("HoldPort(%d): %v", port, err)
	}
	defer holder.Close()
	if holder.tcpFD6 >= 0 {
		_ = syscall.Close(holder.tcpFD6)
		holder.tcpFD6 = -1
	}
	_ = holder.TransitionToTimeWait()

	assertRawBind4(t, port, [4]byte{127, 0, 0, 1}, "127.0.0.1")
	assertRawBind4(t, port, [4]byte{0, 0, 0, 0}, "0.0.0.0")
}

func TestHoldPort_TransitionToTimeWait_placesGuardWhenNoPriorTimeWaitExists(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	kc := zlock.NewGlobalKeychain(t.TempDir())

	res, err := ReservePort(kc, nil)
	if err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	port := res.Port()
	res.Close()

	holder, err := HoldPort(kc, port)
	if err != nil {
		t.Fatalf("HoldPort(%d): %v", port, err)
	}
	defer holder.Close()

	if !holder.TransitionToTimeWait() {
		t.Fatalf("holder.TransitionToTimeWait(%d) = false, want true", port)
	}
	if tryBindLoopbackProbe(port) {
		t.Fatalf("tryBindLoopbackProbe(%d) succeeded after HoldPort.TransitionToTimeWait with no prior TIME_WAIT; want [::1] TIME_WAIT guard placed", port)
	}
}

func TestLease_lifecycleAndNilPanics(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	kc := zlock.NewGlobalKeychain(t.TempDir())

	var nilLease *Lease
	assertPanics(t, "nil.HandOff()", func() { nilLease.HandOff(nil) })
	assertPanics(t, "nil.Close()", func() { nilLease.Close() })

	res, err := ReservePort(kc, nil)
	if err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	port := res.Port()
	res.Close()

	lease := HoldPorts(kc, []int{0, -1, 70000, port, port}, func(p int, err error) {
		t.Fatalf("unexpected HoldPorts error on port %d: %v", p, err)
	})
	defer lease.Close()

	if _, err := kc.TryLockPort(port); !errors.Is(err, zlock.ErrLocked) {
		t.Fatalf("TryLockPort(%d) while Lease active = %v, want ErrLocked", port, err)
	}

	lease.HandOff(nil)

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("Listen(%d) after Lease.HandOff: %v", port, err)
	}
	_ = ln.Close()

	lease.Close()
	unlock, err := kc.TryLockPort(port)
	if err != nil {
		t.Fatalf("TryLockPort(%d) after Lease.Close: %v", port, err)
	}
	unlock()
}

func TestPicker_lifecycleAndNilPanics(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	kc := zlock.NewGlobalKeychain(t.TempDir())

	var nilPicker *Picker
	assertPanics(t, "nil.SetKeychain()", func() { nilPicker.SetKeychain(kc) })
	assertPanics(t, "nil.Exclude()", func() { nilPicker.Exclude(30000) })
	assertPanics(t, "nil.Pick()", func() { _, _ = nilPicker.Pick("svc") })
	assertPanics(t, "nil.ClaimProbePort()", func() { nilPicker.ClaimProbePort("svc", 30000) })
	assertPanics(t, "nil.PortsFor()", func() { _ = nilPicker.PortsFor("svc") })
	assertPanics(t, "nil.Finish()", func() { nilPicker.Finish(true) })
	assertPanics(t, "nil.HandOff()", func() { nilPicker.HandOff() })
	assertPanics(t, "nil.Close()", func() { nilPicker.Close() })

	p := NewPicker(kc)
	assertPanics(t, "HandOff before Finish", func() { p.HandOff() })
	p1, err := p.Pick("svc-a")
	if err != nil {
		t.Fatalf("Pick(svc-a): %v", err)
	}
	assertPanics(t, "SetKeychain after Pick", func() { p.SetKeychain(kc) })
	unowned1, err := p.Pick("")
	if err != nil {
		t.Fatalf("Pick(unowned1): %v", err)
	}
	unowned2, err := p.Pick("")
	if err != nil {
		t.Fatalf("Pick(unowned2): %v", err)
	}
	p.ClaimProbePort("svc-a", unowned1)
	p.ClaimProbePort("svc-a", unowned2)
	p.ClaimProbePort("svc-b", unowned2)

	portsA := p.PortsFor("svc-a")
	if len(portsA) != 2 {
		t.Fatalf("PortsFor(svc-a) = %v, want 2 ports (%d and %d)", portsA, p1, unowned1)
	}
	if len(p.PortsFor("svc-b")) != 0 {
		t.Fatalf("PortsFor(svc-b) = %v, want empty (shared probe port must not be claimed)", p.PortsFor("svc-b"))
	}
	p.Finish(true)
	assertPanics(t, "Pick after Finish", func() { _, _ = p.Pick("svc-a") })
	assertPanics(t, "Exclude after Finish", func() { p.Exclude(30000) })
	assertPanics(t, "ClaimProbePort after Finish", func() { p.ClaimProbePort("svc-a", unowned1) })
	assertPanics(t, "Finish twice", func() { p.Finish(true) })
	p.HandOff()
	assertPanics(t, "HandOff twice", func() { p.HandOff() })
	assertPanics(t, "PortsFor after Close", func() { _ = p.PortsFor("svc-a") })
	p.Close()
}

func runStressWorkerCycle(t *testing.T, kc zlock.GlobalKeychain, skip map[int]bool, activeOwners *[65536]atomic.Int32, totalCycles *atomic.Int64) {
	t.Helper()
	res, err := ReservePort(kc, skip)
	if err != nil {
		t.Errorf("ReservePort: %v", err)
		return
	}
	port := res.Port()
	if prev := activeOwners[port].Swap(1); prev != 0 {
		res.Close()
		t.Errorf("RACE CONDITION: port %d allocated to two workers simultaneously", port)
		return
	}
	defer activeOwners[port].Store(0)

	activeOwners[port].Store(2)
	_ = res.TransitionToTimeWait()
	res.Close()
	runtime.Gosched()

	holder, err := HoldPort(kc, port)
	if err != nil {
		t.Errorf("HoldPort(%d) after TransitionToTimeWait: %v", port, err)
		return
	}
	defer holder.Close()
	activeOwners[port].Store(3)

	time.Sleep(5 * time.Millisecond)

	activeOwners[port].Store(4)
	_ = holder.TransitionToTimeWait()
	runtime.Gosched()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Errorf("service Listen(%d) after TransitionToTimeWait: %v", port, err)
		return
	}
	activeOwners[port].Store(0)
	_ = ln.Close()
	totalCycles.Add(1)
}

func buildSkipOutsideWindow(winOffset, windowSize, stride int) map[int]bool {
	allowed := make(map[int]bool, windowSize)
	for i := range windowSize {
		allowed[PortRangeBase+(winOffset+i*stride)%PortRangeSpan] = true
	}
	skip := make(map[int]bool, PortRangeSpan-windowSize)
	for i := range PortRangeSpan {
		p := PortRangeBase + i
		if !allowed[p] {
			skip[p] = true
		}
	}
	return skip
}

func tryStealReservedPort(kc zlock.GlobalKeychain, port int) bool {
	unlock, err := kc.TryLockPort(port)
	if err != nil {
		return false
	}
	defer unlock()
	return tryBindLoopbackProbe(port)
}

func tryBindLoopbackProbe(port int) bool {
	tcpFD, tcpFD6, udpFD, udpFD6, ok := bindLoopbackProbe(port)
	if !ok {
		return false
	}
	r := &Reservation{port: port, state: stateBound, tcpFD: tcpFD, tcpFD6: tcpFD6, udpFD: udpFD, udpFD6: udpFD6}
	r.Close()
	return true
}

func assertRawBind4(t *testing.T, port int, addr [4]byte, label string) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socket(AF_INET) for %s: %v", label, err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: addr}); err != nil {
		t.Fatalf("raw SO_REUSEADDR=0 Bind(%s:%d) failed: %v", label, port, err)
	}
}

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", name)
		}
	}()
	fn()
}
