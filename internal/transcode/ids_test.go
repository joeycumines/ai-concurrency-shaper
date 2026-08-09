package transcode

// Review-08 blocker 6 regression tests: exchange IDs are API object
// identifiers, not internal indexes — every exchange must emit
// collision-resistant IDs so clients keying response stores, tool-call
// correlation, logs, and retry/dedup systems by these IDs never collide.

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestGeneratedIDsAreUniqueAcrossExchanges proves IDs emitted by independent
// exchanges never collide: the random per-exchange prefix makes
// cross-exchange collisions effectively impossible, while the local counter
// keeps ordering within one exchange monotonic (review-08 blocker 6). The
// emitted shape is msg_<16 lowercase hex>_<counter>.
func TestGeneratedIDsAreUniqueAcrossExchanges(t *testing.T) {
	const (
		exchanges   = 4096
		perExchange = 8
	)
	var (
		mu   sync.Mutex
		seen = make(map[string]struct{}, exchanges*perExchange)
	)
	var shapeMismatch atomic.Bool

	checkShape := func(id string, counter int) bool {
		parts := strings.Split(id, "_")
		if len(parts) != 3 || parts[0] != "msg" {
			return false
		}
		if len(parts[1]) != 16 {
			return false
		}
		for _, r := range parts[1] {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return false
			}
		}
		return parts[2] == strconv.Itoa(counter)
	}

	var wg sync.WaitGroup
	for i := 0; i < exchanges; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids := NewExchangeIDs()
			for j := 1; j <= perExchange; j++ {
				id := ids.New("msg_")
				if !checkShape(id, j) {
					shapeMismatch.Store(true)
					return
				}
				mu.Lock()
				if _, dup := seen[id]; dup {
					mu.Unlock()
					t.Errorf("duplicate ID %q across exchanges", id)
					return
				}
				seen[id] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if shapeMismatch.Load() {
		t.Fatal("ID does not match the documented msg_<16 hex>_<counter> shape")
	}
	if len(seen) != exchanges*perExchange {
		t.Fatalf("collected %d IDs, want %d", len(seen), exchanges*perExchange)
	}
}
