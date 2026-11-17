package znet

import (
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zlock"
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
	const (
		stressDuration = 60 * time.Second
		burstInterval  = 200 * time.Millisecond
		workers        = 8
		windowSize     = 10
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
	currentBase.Store(PortRangeBase)

	for s := range stealers {
		stealerWG.Add(1)
		go func(id int) {
			defer stealerWG.Done()
			var lc net.ListenConfig
			for !stopStealers.Load() {
				if id%2 == 0 {
					ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
					if err == nil {
						_ = ln.Close()
					}
				} else {
					base := int(currentBase.Load())
					for offset := range windowSize {
						p := PortRangeBase + (base-PortRangeBase+offset)%PortRangeSpan
						phaseBefore := activeOwners[p].Load()
						if phaseBefore == 0 {
							continue
						}
						if tryStealReservedPort(kc, p) {
							if phaseAfter := activeOwners[p].Load(); phaseAfter != 0 {
								t.Errorf("RACE CONDITION: stealer bound reserved port %d while owned by worker (phaseBefore=%d phaseAfter=%d)", p, phaseBefore, phaseAfter)
								return
							}
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
		winOffset := (burst * windowSize) % PortRangeSpan
		currentBase.Store(int32(PortRangeBase + winOffset))
		skip := buildSkipOutsideWindow(winOffset, windowSize)

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

	res.ReleaseToTimeWait()
	activeOwners[port].Store(2)
	runtime.Gosched()

	holder, err := HoldPort(kc, port)
	if err != nil {
		t.Errorf("HoldPort(%d) after ReleaseToTimeWait: %v", port, err)
		return
	}
	defer holder.Close()
	activeOwners[port].Store(3)

	time.Sleep(5 * time.Millisecond)

	holder.TransitionToTimeWait()
	activeOwners[port].Store(4)
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

func buildSkipOutsideWindow(winOffset, windowSize int) map[int]bool {
	allowed := make(map[int]bool, windowSize)
	for i := range windowSize {
		allowed[PortRangeBase+(winOffset+i)%PortRangeSpan] = true
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
