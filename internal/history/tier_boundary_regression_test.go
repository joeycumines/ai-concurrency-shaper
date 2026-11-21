package history

import (
	"context"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

func TestTierFetchBoundaryAndExclusionRegression(t *testing.T) {
	s, now, _ := testStore(t)
	for i := range 3600 {
		at := now.Add(-2*time.Hour + time.Duration(i)*time.Second)
		commitPacket(t, s, observation.Packet{Intervals: []observation.Interval{sampleInterval(at, uint64(i+1), time.Second)}})
	}
	// Partial-hour query with preferred=3600 must retain boundary children.
	q := observation.Query{From: now.Add(-90 * time.Minute), To: now.Add(-30 * time.Minute), Providers: []string{"north"}, MaxPoints: 1}
	rows, _, _, err := s.Read(context.Background(), q, observation.Exclusion{})
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, r := range rows {
		total += r.Counts.Terminal
	}
	if total < 1 {
		t.Fatalf("boundary query lost all data: %d rows %d", len(rows), total)
	}
	// Excluded tail seam must include children.
	q = observation.Query{From: now.Add(-2 * time.Hour), To: now, Providers: []string{"north"}, MaxPoints: 1}
	rows, _, _, err = s.Read(context.Background(), q, observation.Exclusion{Epoch: rowsEpoch(s), FromSequence: 5000})
	if err != nil {
		t.Fatal(err)
	}
	total = 0
	for _, r := range rows {
		total += r.Counts.Terminal
	}
	if total == 0 {
		t.Fatal("exclusion seam lost all data")
	}
}
func rowsEpoch(s *Store) string {
	var epoch string
	_ = s.writer.QueryRow("SELECT epoch FROM receipts LIMIT 1").Scan(&epoch)
	return epoch
}
