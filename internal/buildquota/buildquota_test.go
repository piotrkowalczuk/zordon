package buildquota

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaxCPU_Set(t *testing.T) {
	cases := map[string]struct {
		input   string
		want    MaxCPU
		wantErr bool
	}{
		"empty":       {input: "", want: 0},
		"zero":        {input: "0", want: 0},
		"positive":    {input: "6", want: 6},
		"trimmed":     {input: "  3 ", want: 3},
		"negative":    {input: "-1", wantErr: true},
		"non-numeric": {input: "auto", wantErr: true},
		"float":       {input: "2.5", wantErr: true},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			var m MaxCPU
			err := m.Set(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) succeeded with %d, want error", c.input, m)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q) error: %v", c.input, err)
			}
			if m != c.want {
				t.Fatalf("Set(%q) = %d, want %d", c.input, m, c.want)
			}
		})
	}
}

func TestMaxCPU_Resolve(t *testing.T) {
	cases := map[string]struct {
		maxCPU        MaxCPU
		availableCPUs int
		want          int
	}{
		"auto-1-core":     {maxCPU: 0, availableCPUs: 1, want: 1},
		"auto-2-cores":    {maxCPU: 0, availableCPUs: 2, want: 2},
		"auto-4-cores":    {maxCPU: 0, availableCPUs: 4, want: 4},
		"auto-5-cores":    {maxCPU: 0, availableCPUs: 5, want: 3},
		"auto-8-cores":    {maxCPU: 0, availableCPUs: 8, want: 6},
		"auto-12-cores":   {maxCPU: 0, availableCPUs: 12, want: 10},
		"explicit-1":      {maxCPU: 1, availableCPUs: 12, want: 1},
		"explicit-4-on-2": {maxCPU: 4, availableCPUs: 2, want: 4},
		"explicit-6-on-8": {maxCPU: 6, availableCPUs: 8, want: 6},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := c.maxCPU.Resolve(c.availableCPUs); got != c.want {
				t.Fatalf("MaxCPU(%d).Resolve(%d) = %d, want %d", c.maxCPU, c.availableCPUs, got, c.want)
			}
		})
	}
}

func TestLimiter_Acquire_serializesAndPreservesFIFO(t *testing.T) {
	lim := NewLimiter(3)

	rel1, q1, ok := lim.TryAcquire()
	if !ok || q1 != 3 {
		t.Fatalf("TryAcquire() = (%v, %d), want (true, 3)", ok, q1)
	}
	if _, _, ok2 := lim.TryAcquire(); ok2 {
		t.Fatal("second TryAcquire succeeded while slot held")
	}

	var (
		inFlight    atomic.Int32
		maxInFlight atomic.Int32
		orderMu     sync.Mutex
		order       []int
	)

	ready2 := make(chan struct{})
	ready3 := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		close(ready2)
		rel, q, err := lim.Acquire(t.Context())
		if err != nil {
			t.Errorf("worker 2 Acquire: %v", err)
			return
		}
		defer rel()
		if q != 3 {
			t.Errorf("worker 2 quota = %d, want 3", q)
		}
		recordConcurrency(&inFlight, &maxInFlight)
		orderMu.Lock()
		order = append(order, 2)
		orderMu.Unlock()
		time.Sleep(25 * time.Millisecond)
		inFlight.Add(-1)
	}()

	<-ready2
	waitForQueueLen(t, lim, 1)

	go func() {
		defer wg.Done()
		close(ready3)
		rel, q, err := lim.Acquire(t.Context())
		if err != nil {
			t.Errorf("worker 3 Acquire: %v", err)
			return
		}
		defer rel()
		if q != 3 {
			t.Errorf("worker 3 quota = %d, want 3", q)
		}
		recordConcurrency(&inFlight, &maxInFlight)
		orderMu.Lock()
		order = append(order, 3)
		orderMu.Unlock()
		inFlight.Add(-1)
	}()

	<-ready3
	waitForQueueLen(t, lim, 2)

	rel1()
	wg.Wait()

	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("max concurrent holders = %d, want 1", got)
	}
	if !slices.Equal(order, []int{2, 3}) {
		t.Fatalf("wakeup order = %v, want [2 3]", order)
	}
}

func TestLimiter_Acquire_respectsContextCancellation(t *testing.T) {
	var lim Limiter
	rel1, _, err := lim.Acquire(t.Context())
	if err != nil {
		t.Fatalf("initial Acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := lim.Acquire(ctx)
		errCh <- err
	}()

	waitForQueueLen(t, &lim, 1)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Acquire did not unblock")
	}

	rel1()

	rel2, q2, ok := lim.TryAcquire()
	if !ok || q2 < 1 {
		t.Fatalf("TryAcquire after cancelled waiter = (%v, %d), want (true, >=1)", ok, q2)
	}
	rel2()
}

func TestLimiter_SetMaxCPU_updatesWeightedCapacitySafelyMidFlight(t *testing.T) {
	lim := NewLimiter(2)
	rel1, q1, ok := lim.TryAcquire()
	if !ok || q1 != 2 {
		t.Fatalf("initial TryAcquire = (%v, %d), want (true, 2)", ok, q1)
	}

	lim.SetMaxCPU(5)
	if got := lim.Quota(); got != 5 {
		t.Fatalf("Quota() after SetMaxCPU(5) = %d, want 5", got)
	}

	rel2, q2, err := lim.Acquire(t.Context())
	if err != nil {
		t.Fatalf("Acquire after SetMaxCPU(5): %v", err)
	}
	if q2 != 5 {
		t.Fatalf("quota after SetMaxCPU(5) = %d, want 5", q2)
	}
	rel1()
	rel2()
}

func TestWithBuildEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "CARGO_BUILD_JOBS=1", "MAKEFLAGS="}
	got := envSliceToMap(WithBuildEnv(base, 6))

	if got["CARGO_BUILD_JOBS"] != "1" {
		t.Errorf("CARGO_BUILD_JOBS = %q, want explicit 1 preserved", got["CARGO_BUILD_JOBS"])
	}
	if v, ok := got["MAKEFLAGS"]; !ok || v != "" {
		t.Errorf("MAKEFLAGS = %q (present=%v), want explicit empty value preserved", v, ok)
	}
	if got["BUNDLE_JOBS"] != "6" {
		t.Errorf("BUNDLE_JOBS = %q, want 6", got["BUNDLE_JOBS"])
	}
	if got["CMAKE_BUILD_PARALLEL_LEVEL"] != "6" {
		t.Errorf("CMAKE_BUILD_PARALLEL_LEVEL = %q, want 6", got["CMAKE_BUILD_PARALLEL_LEVEL"])
	}
	if got["GOMAXPROCS"] != "6" {
		t.Errorf("GOMAXPROCS = %q, want 6", got["GOMAXPROCS"])
	}

	gotDefault := envSliceToMap(WithBuildEnv(nil, 4))
	if gotDefault["MAKEFLAGS"] != "-j4" {
		t.Errorf("default MAKEFLAGS = %q, want -j4", gotDefault["MAKEFLAGS"])
	}
}

func recordConcurrency(inFlight, maxInFlight *atomic.Int32) {
	cur := inFlight.Add(1)
	for {
		old := maxInFlight.Load()
		if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
			return
		}
	}
}

func waitForQueueLen(t *testing.T, lim *Limiter, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lim.Waiters() == want {
			time.Sleep(5 * time.Millisecond)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for limiter waiters = %d (got %d)", want, lim.Waiters())
}

func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}
