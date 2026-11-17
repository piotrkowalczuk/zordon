package znet

import (
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

	if fd, fd6, ok := bindAndListenProbe(port); ok {
		r := &Reservation{port: port, listenFD: fd, listenFD6: fd6}
		r.Close()
		res.Close()
		t.Fatalf("bindAndListenProbe(%d) succeeded while Reservation is in LISTEN", port)
	}

	res.ReleaseToTimeWait()
	res.ReleaseToTimeWait()

	if fd, fd6, ok := bindAndListenProbe(port); ok {
		r := &Reservation{port: port, listenFD: fd, listenFD6: fd6}
		r.Close()
		t.Fatalf("bindAndListenProbe(%d) succeeded while port is in TIME_WAIT", port)
	}

	holder, err := HoldPort(kc, port)
	if err != nil {
		t.Fatalf("HoldPort(%d) over TIME_WAIT: %v", port, err)
	}
	defer holder.Close()

	if fd, fd6, ok := bindAndListenProbe(port); ok {
		r := &Reservation{port: port, listenFD: fd, listenFD6: fd6}
		r.Close()
		t.Fatalf("bindAndListenProbe(%d) succeeded while HoldPort is active", port)
	}

	holder.TransitionToTimeWait()

	if fd, fd6, ok := bindAndListenProbe(port); ok {
		r := &Reservation{port: port, listenFD: fd, listenFD6: fd6}
		r.Close()
		t.Fatalf("bindAndListenProbe(%d) succeeded after HoldPort TransitionToTimeWait", port)
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
						stolen = tryBindAndListenProbe(p)
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
		r.TransitionToTimeWait()
	})
	assertPanics(t, "nil.ReleaseToTimeWait", func() {
		var r *Reservation
		r.ReleaseToTimeWait()
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
		_ = syscall.Dup2(saved0, 0)
		_ = syscall.Close(saved0)
	}()

	var zero Reservation
	zero.TransitionToTimeWait()
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

func TestReservation_TransitionToTimeWait_leakedServerFDDoesNotHang(t *testing.T) {
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
	res.ReleaseToTimeWait()

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(4)
	for range 4 {
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if tryBindAndListenProbe(port) {
					t.Errorf("tryBindAndListenProbe(%d) succeeded while [::1]:%d is in TIME_WAIT", port, port)
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

func TestReservation_TransitionToTimeWait_fallsBackToIPv4WhenIPv6Unavailable(t *testing.T) {
	ztest.AssertSystem(t, ztest.LoopbackConnect)
	kc := zlock.NewGlobalKeychain(t.TempDir())

	res, err := ReservePort(kc, nil)
	if err != nil {
		t.Fatalf("ReservePort: %v", err)
	}
	port := res.Port()
	if res.listenFD6 >= 0 {
		_ = syscall.Close(res.listenFD6)
		res.listenFD6 = -1
	}
	res.ReleaseToTimeWait()

	if tryBindAndListenProbe(port) {
		t.Fatalf("tryBindAndListenProbe(%d) succeeded when IPv6 was unavailable; want IPv4 TIME_WAIT fallback to block SO_REUSEADDR=0 probe", port)
	}
}

func TestBindAndListenTCP6Probe_rejectsSocketEMFILE(t *testing.T) {
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

	fd6, ok := bindAndListenTCP6Probe(PortRangeBase)
	if fd6 >= 0 {
		_ = syscall.Close(fd6)
	}
	if ok {
		t.Fatal("bindAndListenTCP6Probe returned ok=true on EMFILE; want ok=false so transient fd exhaustion does not bypass IPv6 probe")
	}
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
	res.ReleaseToTimeWait()
	runtime.Gosched()

	holder, err := HoldPort(kc, port)
	if err != nil {
		t.Errorf("HoldPort(%d) after ReleaseToTimeWait: %v", port, err)
		return
	}
	defer holder.Close()
	activeOwners[port].Store(3)

	time.Sleep(5 * time.Millisecond)

	activeOwners[port].Store(4)
	holder.TransitionToTimeWait()
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
	return tryBindAndListenProbe(port)
}

func tryBindAndListenProbe(port int) bool {
	fd, fd6, ok := bindAndListenProbe(port)
	if !ok {
		return false
	}
	r := &Reservation{port: port, listenFD: fd, listenFD6: fd6}
	r.Close()
	return true
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
