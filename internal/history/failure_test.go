package history

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

func TestWriterFailureRecoveryAndCloseReporting(t *testing.T) {
	now := time.Now()
	s, err := Open(t.TempDir()+"/writer.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if _, err = s.writer.Exec(`CREATE TRIGGER fail_intervals BEFORE INSERT ON receipts BEGIN SELECT RAISE(ABORT,'writer deliberately blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if !s.Enqueue(observation.Packet{Intervals: []observation.Interval{sampleInterval(now, 1, time.Second)}}) {
		t.Fatal("enqueue failed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.Health().WriteErrors == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	h := s.Health()
	if h.WriteErrors != 1 || h.Healthy {
		t.Fatalf("writer failure not reported: %+v", h)
	}
	if _, err = s.writer.Exec("DROP TRIGGER fail_intervals"); err != nil {
		t.Fatal(err)
	}
	if !s.Enqueue(observation.Packet{Intervals: []observation.Interval{sampleInterval(now.Add(2*time.Second), 2, time.Second)}}) {
		t.Fatal("recovery enqueue failed")
	}
	deadline = time.Now().Add(5 * time.Second)
	for !s.Health().Healthy && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	h = s.Health()
	if !h.Healthy || h.WriteErrors != 1 || h.CommittedThrough.IsZero() {
		t.Fatalf("recovery=%+v", h)
	}
	rows, markers, _, err := s.Read(context.Background(), observation.Query{From: now.Add(-time.Second), To: now.Add(4 * time.Second), Providers: []string{"north"}, MaxPoints: 10}, observation.Exclusion{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Sequence != 2 || len(markers) != 1 || markers[0].Kind != "recorder_gap" {
		t.Fatalf("failure falsely persisted or marker disappeared: rows=%+v markers=%+v", rows, markers)
	}
	if err = s.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "writer deliberately blocked") {
		t.Fatalf("shutdown hid writer error: %v", err)
	}
}
func TestShutdownTimeoutDoesNotAbandonDrain(t *testing.T) {
	s, now, _ := testStore(t)
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if !s.Enqueue(observation.Packet{Intervals: []observation.Interval{sampleInterval(now.Add(-time.Second), 1, time.Second)}}) {
		t.Fatal("enqueue")
	}
	deadline := time.Now().Add(time.Second)
	for s.writer.Stats().WaitCount == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = s.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown timeout=%v", err)
	}
	if s.Enqueue(observation.Packet{}) {
		t.Fatal("shutdown accepted new packet")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Health().CommittedThrough.IsZero() {
		t.Fatal("timed out close abandoned queued data")
	}
}
func TestExpiredRawCannotRebuildAndLoseOldRollup(t *testing.T) {
	s, now, _ := testStore(t)
	start := now.Add(-2 * time.Hour)
	first := sampleInterval(start, 1, time.Second)
	commitPacket(t, s, observation.Packet{Intervals: []observation.Interval{first}})
	if count(t, s, "buckets") != 0 {
		t.Fatal("expired raw retained")
	}
	second := sampleInterval(start.Add(3*time.Second), 2, time.Second)
	second.ClockStep = true
	commitPacket(t, s, observation.Packet{Intervals: []observation.Interval{second}})
	commitPacket(t, s, observation.Packet{Intervals: []observation.Interval{first}})
	q := observation.Query{From: start, To: start.Add(time.Minute), MaxPoints: 1}
	rows, _, _, err := s.Read(context.Background(), q, observation.Exclusion{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Counts.Terminal != 2 || rows[0].Samples != 2 || !rows[0].Gap || !rows[0].ClockStep {
		t.Fatalf("incomplete raw replaced absolute rollup: %+v", rows)
	}
}
func TestReadDecodeFailureReleasesConnections(t *testing.T) {
	s, now, _ := testStore(t)
	v := sampleInterval(now.Add(-time.Second), 1, time.Second)
	commitPacket(t, s, observation.Packet{Intervals: []observation.Interval{v}})
	if _, err := s.writer.Exec("UPDATE buckets SET payload=?", []byte(`{"invalid`)); err != nil {
		t.Fatal(err)
	}
	q := observation.Query{From: v.Start, To: v.End, MaxPoints: 100}
	for range 4 {
		if _, _, _, err := s.Read(context.Background(), q, observation.Exclusion{}); err == nil {
			t.Fatal("invalid stored JSON accepted")
		}
		if stats := s.reader.Stats(); stats.InUse != 0 {
			t.Fatalf("query leaked pooled connection: %+v", stats)
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.writer.Exec("UPDATE buckets SET payload=?", raw); err != nil {
		t.Fatal(err)
	}
	if rows, _, _, err := s.Read(context.Background(), q, observation.Exclusion{}); err != nil || len(rows) != 1 {
		t.Fatalf("subsequent query blocked or failed: %v", err)
	}
}
