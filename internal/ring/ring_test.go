package ring

import (
	"reflect"
	"testing"
)

func TestNew_zeroCapPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New(0) did not panic")
		}
	}()
	_ = New(0)
}

func TestBuffer_empty(t *testing.T) {
	b := New(3)
	if b.Len() != 0 {
		t.Fatalf("Len=%d, want 0", b.Len())
	}
	if got := b.Dump(); len(got) != 0 {
		t.Fatalf("Dump=%v, want empty", got)
	}
}

func TestBuffer_fillUnderCapacity(t *testing.T) {
	b := New(5)
	b.Push("a")
	b.Push("b")
	b.Push("c")
	want := []string{"a", "b", "c"}
	if got := b.Dump(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Dump=%v, want %v", got, want)
	}
	if b.Len() != 3 {
		t.Fatalf("Len=%d, want 3", b.Len())
	}
}

func TestBuffer_exactlyFull(t *testing.T) {
	b := New(3)
	b.Push("a")
	b.Push("b")
	b.Push("c")
	want := []string{"a", "b", "c"}
	if got := b.Dump(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Dump=%v, want %v", got, want)
	}
}

func TestBuffer_overflowDropsOldest(t *testing.T) {
	b := New(3)
	b.Push("a")
	b.Push("b")
	b.Push("c")
	b.Push("d") // drops "a"
	b.Push("e") // drops "b"
	want := []string{"c", "d", "e"}
	if got := b.Dump(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Dump=%v, want %v", got, want)
	}
	if b.Len() != 3 {
		t.Fatalf("Len=%d, want 3", b.Len())
	}
}

func TestBuffer_multipleWrapsKeepsLastCap(t *testing.T) {
	b := New(3)
	for i := range 100 {
		b.Push(string(rune('a' + (i % 26))))
	}
	got := b.Dump()
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3", len(got))
	}
	// Last three pushes were i=97,98,99 → chars (97%26,98%26,99%26)
	// = (97-78=19→'t', 20→'u', 21→'v'). Sanity-check ordering.
	want := []string{"t", "u", "v"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestBuffer_Dump_isCopy(t *testing.T) {
	b := New(3)
	b.Push("a")
	b.Push("b")
	d := b.Dump()
	d[0] = "MUTATED"
	// Re-dump must still see "a"; if Dump returned the internal slice,
	// a caller's mutation would corrupt subsequent reads.
	if got := b.Dump(); got[0] != "a" {
		t.Fatalf("Dump returned internal slice; got %v", got)
	}
}

func TestBuffer_Cap(t *testing.T) {
	if c := New(7).Cap(); c != 7 {
		t.Fatalf("Cap=%d, want 7", c)
	}
}
