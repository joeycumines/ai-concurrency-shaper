package observation

import (
	"context"
	"testing"
	"time"
)

func TestTickEvictionKeepsWholeSequences(t *testing.T) {
	h, c := newTestHub(t, "one", "two", "three")
	for range 12000/4 + 2 {
		c.Advance(time.Second)
		h.Tick()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.closed) == 0 {
		t.Fatal("all intervals evicted")
	}
	firstSeq := h.closed[0].Sequence
	for _, iv := range h.closed {
		if iv.Sequence < firstSeq {
			t.Fatal("eviction left an older sequence behind")
		}
	}
	// Every sequence retained in memory must have one interval per provider.
	counts := make(map[uint64]int)
	for _, iv := range h.closed {
		counts[iv.Sequence]++
	}
	for seq, n := range counts {
		if n != len(h.providers) {
			t.Fatalf("sequence %d has %d of %d providers", seq, n, len(h.providers))
		}
	}
	_ = context.Background()
}
