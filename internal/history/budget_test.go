package history

import (
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

func TestEnqueueByteBudgetIncludesClaimedBatch(t *testing.T) {
	// No database exists: this isolates the request-path admission contract from
	// disk scheduling and proves enqueue cannot require I/O. The writer claims a
	// packet below without completing it, like a stalled transaction.
	s := &Store{queue: make(chan queuedPacket, QueueCapacity), health: observation.Health{Enabled: true, Healthy: true, Capacity: QueueCapacity, ByteCapacity: ByteCapacity}}
	attempts := make([]observation.AttemptFact, 32)
	facts := make([]observation.RequestFact, 5000)
	for i := range facts {
		facts[i] = observation.RequestFact{ID: "request", Provider: "north", Path: strings.Repeat("p", 1024), Route: strings.Repeat("r", 1024), Attempts: attempts}
	}
	packet := observation.Packet{Requests: facts}
	if !s.Enqueue(packet) {
		t.Fatal("valid first packet refused")
	}
	claimed := <-s.queue
	if s.Health().Pending != 0 || s.Health().BufferedBytes == 0 {
		t.Fatal("claimed writer batch escaped accounting")
	}
	if s.Enqueue(packet) {
		t.Fatal("byte budget exceeded while packet count was zero")
	}
	h := s.Health()
	if h.DroppedBatches != 1 || h.DroppedFacts != 5000 || h.BufferedBytes > ByteCapacity || claimed.bytes != h.BufferedBytes {
		t.Fatalf("byte pressure health=%+v", h)
	}
}
