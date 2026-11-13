package buildquota

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

type MaxCPU int

func (m *MaxCPU) String() string {
	if m == nil {
		return "0"
	}
	return strconv.Itoa(int(*m))
}

func (m *MaxCPU) Set(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*m = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return fmt.Errorf("invalid build.maxcpu %q: must be a non-negative integer (0 = auto)", s)
	}
	*m = MaxCPU(n)
	return nil
}

func (m MaxCPU) Int() int {
	if m < 0 {
		panic("buildquota: negative MaxCPU")
	}
	return int(m)
}

func (m MaxCPU) Resolve(availableCPUs int) int {
	if m < 0 {
		panic("buildquota: negative MaxCPU")
	}
	if availableCPUs <= 0 {
		panic("buildquota: availableCPUs must be > 0")
	}
	if m > 0 {
		return int(m)
	}
	if availableCPUs > 4 {
		return availableCPUs - 2
	}
	return availableCPUs
}

func AvailableCPUs() int {
	return max(1, runtime.GOMAXPROCS(0))
}

type Limiter struct {
	mu      sync.Mutex
	maxCPU  MaxCPU
	sem     *semaphore.Weighted
	semSize int
	waiters atomic.Int32
}

func NewLimiter(maxCPU MaxCPU) *Limiter {
	if maxCPU < 0 {
		panic("buildquota: negative MaxCPU")
	}
	q := maxCPU.Resolve(AvailableCPUs())
	return &Limiter{
		maxCPU:  maxCPU,
		sem:     semaphore.NewWeighted(int64(q)),
		semSize: q,
	}
}

func (l *Limiter) SetMaxCPU(maxCPU MaxCPU) {
	if l == nil {
		panic("buildquota: nil Limiter")
	}
	if maxCPU < 0 {
		panic("buildquota: negative MaxCPU")
	}
	l.mu.Lock()
	l.maxCPU = maxCPU
	q := maxCPU.Resolve(AvailableCPUs())
	if l.sem == nil || l.semSize != q {
		l.sem = semaphore.NewWeighted(int64(q))
		l.semSize = q
	}
	l.mu.Unlock()
}

func (l *Limiter) MaxCPU() MaxCPU {
	if l == nil {
		panic("buildquota: nil Limiter")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxCPU
}

func (l *Limiter) Quota() int {
	if l == nil {
		panic("buildquota: nil Limiter")
	}
	_, q := l.semAndQuota()
	return q
}

func (l *Limiter) Waiters() int {
	if l == nil {
		panic("buildquota: nil Limiter")
	}
	return int(l.waiters.Load())
}

func (l *Limiter) TryAcquire() (release func(), quota int, ok bool) {
	if l == nil {
		panic("buildquota: nil Limiter")
	}
	sem, q := l.semAndQuota()
	w := int64(q)
	if !sem.TryAcquire(w) {
		return nil, 0, false
	}
	return makeRelease(sem, w), q, true
}

func (l *Limiter) Acquire(ctx context.Context) (release func(), quota int, err error) {
	if l == nil {
		panic("buildquota: nil Limiter")
	}
	if ctx == nil {
		panic("buildquota: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	sem, q := l.semAndQuota()
	w := int64(q)
	l.waiters.Add(1)
	err = sem.Acquire(ctx, w)
	l.waiters.Add(-1)
	if err != nil {
		return nil, 0, err
	}
	return makeRelease(sem, w), q, nil
}

func (l *Limiter) AcquireWithPhase(ctx context.Context, setPhase func(string), waitPhase, activePhase string) (release func(), quota int, err error) {
	if release, quota, ok := l.TryAcquire(); ok {
		if setPhase != nil {
			setPhase(activePhase)
		}
		return release, quota, nil
	}
	if setPhase != nil {
		setPhase(waitPhase)
	}
	release, quota, err = l.Acquire(ctx)
	if err != nil {
		return nil, 0, err
	}
	if setPhase != nil {
		setPhase(activePhase)
	}
	return release, quota, nil
}

func WithBuildEnv(env []string, quota int) []string {
	if quota <= 0 {
		panic("buildquota: quota must be > 0")
	}
	q := strconv.Itoa(quota)
	defaults := [][2]string{
		{"CARGO_BUILD_JOBS", q},
		{"BUNDLE_JOBS", q},
		{"MAKEFLAGS", "-j" + q},
		{"CMAKE_BUILD_PARALLEL_LEVEL", q},
		{"GOMAXPROCS", q},
	}
	out := slices.Clone(env)
	for _, kv := range defaults {
		if !hasEnvKey(out, kv[0]) {
			out = append(out, kv[0]+"="+kv[1])
		}
	}
	return out
}

func (l *Limiter) semAndQuota() (*semaphore.Weighted, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sem == nil {
		q := l.maxCPU.Resolve(AvailableCPUs())
		l.sem = semaphore.NewWeighted(int64(q))
		l.semSize = q
	}
	return l.sem, l.semSize
}

func makeRelease(sem *semaphore.Weighted, weight int64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			sem.Release(weight)
		})
	}
}

func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}
