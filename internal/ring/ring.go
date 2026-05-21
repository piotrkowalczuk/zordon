// Package ring is a fixed-capacity string ring buffer with
// chronological replay. Used by zordon to keep the last N stdout/
// stderr lines per service during bringup so that on failure we can
// print a focused tail right next to the "what failed and why" verdict
// — instead of asking the user to grep back through a streaming log.
//
// Not safe for concurrent use; the consumer (zordon's pushConfigure
// event loop) is single-goroutine.
package ring

// Buffer holds up to Cap most-recent strings, oldest dropped on
// overflow. Zero value is unusable; construct with New.
type Buffer struct {
	cap    int
	lines  []string
	next   int  // next write index
	filled bool // true once we have wrapped at least once
}

// New returns a buffer of the given capacity. cap<=0 panics — that's
// a programmer error, not a runtime condition (a "zero-line tail" has
// no meaning and silently swallowing input would mask a bug).
func New(cap int) *Buffer {
	if cap <= 0 {
		panic("ring.New: cap must be > 0")
	}
	return &Buffer{cap: cap, lines: make([]string, cap)}
}

// Push appends s; once the buffer is full the oldest entry is
// overwritten. Constant time.
func (b *Buffer) Push(s string) {
	b.lines[b.next] = s
	b.next++
	if b.next == b.cap {
		b.next = 0
		b.filled = true
	}
}

// Len returns the number of stored lines (<= Cap).
func (b *Buffer) Len() int {
	if b.filled {
		return b.cap
	}
	return b.next
}

// Cap returns the capacity passed to New.
func (b *Buffer) Cap() int { return b.cap }

// Dump returns the stored lines oldest-first. The returned slice is a
// fresh copy — safe to retain, mutate, or pass to formatters.
func (b *Buffer) Dump() []string {
	if !b.filled {
		out := make([]string, b.next)
		copy(out, b.lines[:b.next])
		return out
	}
	out := make([]string, 0, b.cap)
	out = append(out, b.lines[b.next:]...)
	out = append(out, b.lines[:b.next]...)
	return out
}
