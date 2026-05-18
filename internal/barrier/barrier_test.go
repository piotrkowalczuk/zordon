package barrier

import (
	"testing"
	"time"
)

func TestBarrier_triggerIsIdempotent(t *testing.T) {
	b := New()
	if b.Triggered() {
		t.Fatal("fresh barrier reports triggered")
	}
	b.Trigger()
	if !b.Triggered() {
		t.Fatal("after Trigger, Triggered=false")
	}
	// Second call must not panic (sync.Once protects close).
	b.Trigger()
}

func TestBarrier_waitUnblocksAfterTrigger(t *testing.T) {
	b := New()
	go func() {
		time.Sleep(5 * time.Millisecond)
		b.Trigger()
	}()
	select {
	case <-b.Wait():
	case <-time.After(time.Second):
		t.Fatal("Wait did not unblock within 1s of Trigger")
	}
}

func TestBarrier_waitIsImmediateIfAlreadyTriggered(t *testing.T) {
	b := New()
	b.Trigger()
	select {
	case <-b.Wait():
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Wait blocked even though Trigger had fired")
	}
}

func TestAny_firesOnFirstSource(t *testing.T) {
	target := New()
	a, b, c := New(), New(), New()
	Any(target, a, b, c)
	if target.Triggered() {
		t.Fatal("target triggered before any source fired")
	}
	b.Trigger()
	select {
	case <-target.Wait():
	case <-time.After(time.Second):
		t.Fatal("target did not fire after first source triggered")
	}
}

func TestAny_extraSourcesAfterTargetFiresAreNoOps(t *testing.T) {
	target := New()
	a, b := New(), New()
	Any(target, a, b)
	a.Trigger()
	<-target.Wait()
	// Should not panic or misbehave when other sources fire later.
	b.Trigger()
}
