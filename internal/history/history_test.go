package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

func testStore(t *testing.T) (*Store, time.Time, string) {
	t.Helper()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := Open(path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s, now, path
}
func sampleInterval(at time.Time, seq uint64, d time.Duration) observation.Interval {
	return observation.Interval{Samples: 1, Epoch: "process", Sequence: seq, LastSequence: seq, Provider: "north", Start: at, End: at.Add(d), ElapsedNS: int64(d), Counts: observation.Counters{Received: 1, Terminal: 1, Completed: 1}, Gauges: observation.Gauges{Active: observation.Gauge{Known: true, Area: float64(d), DurationNS: int64(d), Min: 1, Max: 1, Last: 1}}}
}
func commitPacket(t *testing.T, s *Store, p observation.Packet) {
	t.Helper()
	if err := s.commit(context.Background(), []observation.Packet{p}); err != nil {
		t.Fatal(err)
	}
}
func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.writer.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestSQLiteReplayIdempotentConflictRollsBack(t *testing.T) {
	s, now, _ := testStore(t)
	v := sampleInterval(now.Add(-time.Minute), 1, time.Second)
	commitPacket(t, s, observation.Packet{Intervals: []observation.Interval{v}})
	commitPacket(t, s, observation.Packet{Intervals: []observation.Interval{v}})
	if count(t, s, "buckets") != 1 || count(t, s, "receipts") != 1 || count(t, s, "rollups") != 2 {
		t.Fatal("replay duplicated intervals")
	}
	bad := v
	bad.Counts.Completed = 7
	if err := s.commit(context.Background(), []observation.Packet{{Intervals: []observation.Interval{sampleInterval(now.Add(-2*time.Second), 2, time.Second), bad}}}); !errors.Is(err, ErrConflictingReplay) {
		t.Fatalf("conflicting replay=%v", err)
	}
	if count(t, s, "buckets") != 1 {
		t.Fatal("conflicting transaction was partially committed")
	}
	rows, _, truncated, err := s.Read(context.Background(), observation.Query{From: v.Start, To: v.End, MaxPoints: 1}, observation.Exclusion{})
	if err != nil || truncated || len(rows) != 1 || rows[0].Counts.Completed != 1 {
		t.Fatalf("absolute interval changed: %v %v %+v", err, truncated, rows)
	}
}
func TestSQLiteForeignDatabaseAndUnsupportedVersion(t *testing.T) {
	for _, foreign := range []string{"CREATE TABLE customer(id INTEGER)", "PRAGMA application_id=12345", "PRAGMA user_version=100"} {
		t.Run(foreign, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "foreign.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(foreign); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			s, err := Open(path, Options{})
			if s != nil {
				_ = s.Close(context.Background())
			}
			if !errors.Is(err, ErrForeignDatabase) {
				t.Fatalf("foreign database accepted: %v", err)
			}
		})
	}
}
func TestSQLiteLocalPathEscapingAndReadOnly(t *testing.T) {
	name := "history # Ω.db"
	if runtime.GOOS != "windows" {
		name = "history # ? Ω.db"
	}
	path := filepath.Join(t.TempDir(), name)
	s, err := Open(path, Options{Durability: "full"})
	if err != nil {
		t.Fatal(err)
	}
	var mode int
	if err = s.writer.QueryRow("PRAGMA synchronous").Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("FULL durability=%d %v", mode, err)
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatalf("permissions=%o", info.Mode().Perm())
	}
	ro, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close(context.Background())
	if ro.Enqueue(observation.Packet{}) {
		t.Fatal("read-only store accepted writes")
	}
	if _, err = ro.reader.Exec("DELETE FROM metadata"); err == nil {
		t.Fatal("read-only query pool mutated data")
	}
	for _, p := range []string{"", "file:history.db?mode=memory", "//host/share/db", `\\server\share\db`, "https://host/db"} {
		if _, err := localDSN(p, false); err == nil {
			t.Fatalf("nonlocal path accepted %q", p)
		}
	}
	dsn, err := localDSN("question?#.db", false)
	if err != nil || !strings.Contains(dsn, "%3F%23") {
		t.Fatalf("URI metacharacter escaping=%s %v", dsn, err)
	}
}
func TestSQLiteRealHubCloseReopenAndNoDoubleOverlay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native-clock.db")
	store, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	hub := observation.New(observation.Config{Providers: []string{"north"}, History: store, ManualTicks: true})
	ctx, r := hub.Begin(context.Background(), observation.BeginInfo{Provider: "north", Method: "POST", Path: "/v1/messages"})
	if observation.FromContext(ctx) != r {
		t.Fatal("identity lost")
	}
	a := r.BeginAttempt(observation.AttemptInitial)
	a.Headers(200)
	a.Finish(observation.OutcomeCompleted)
	r.ReportUsage(observation.Usage{InputKnown: true, Input: 0, OutputKnown: true, Output: 3})
	r.SetJournalID(17)
	r.Finish(observation.FinishInfo{Status: 200, Outcome: observation.OutcomeCompleted})
	hub.Tick()
	before := hub.Snapshot()
	if before.History.Enabled != true {
		t.Fatal("history not attached")
	}
	if err = hub.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ro, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close(context.Background())
	facts, err := ro.Requests(context.Background(), observation.RequestQuery{Limit: 10})
	if err != nil || facts.Total != 1 || facts.Requests[0].ID != r.ID() || facts.Requests[0].JournalID != 17 || facts.Requests[0].Usage.InputKnown != true {
		t.Fatalf("facts=%+v %v", facts, err)
	}
	q := observation.Query{From: time.Now().Add(-time.Minute), To: time.Now().Add(time.Second), Providers: []string{"north"}, MaxPoints: 20}
	rows, _, truncated, err := ro.Read(context.Background(), q, observation.Exclusion{})
	if err != nil || truncated || len(rows) < 1 {
		t.Fatalf("read=%+v %v %v", rows, truncated, err)
	}
	q.From = rows[0].Start
	q.To = rows[len(rows)-1].End
	result, err := observation.Aggregate(context.Background(), q, rows)
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, p := range result.Points {
		total += p.Counts.Terminal
		if !p.Complete {
			t.Fatalf("native monotonic->SQLite clock introduced a false gap: %+v", p)
		}
	}
	if total != 1 {
		t.Fatalf("terminal total=%d", total)
	}
	if ro.Health().CommittedThrough.IsZero() {
		t.Fatal("committed watermark absent")
	}
}
func TestSQLiteTierSelectionAndLiveExclusion(t *testing.T) {
	s, now, _ := testStore(t)
	start := now.Add(-time.Hour)
	var intervals []observation.Interval
	for i := range 3600 {
		intervals = append(intervals, sampleInterval(start.Add(time.Duration(i)*time.Second), uint64(i+1), time.Second))
	}
	commitPacket(t, s, observation.Packet{Intervals: intervals})
	q := observation.Query{From: start, To: now, MaxPoints: 10}
	rows, _, truncated, err := s.Read(context.Background(), q, observation.Exclusion{Epoch: "process", FromSequence: 3301})
	if err != nil || truncated {
		t.Fatalf("query %v %v", err, truncated)
	}
	var n uint64
	for _, r := range rows {
		n += r.Counts.Terminal
		if r.LastSequence >= 3301 {
			t.Fatal("persisted tail overlaps memory")
		}
	}
	if n != 3300 {
		t.Fatalf("tail exclusion lost older rollup children: %d", n)
	}
	if count(t, s, "buckets") != 900 {
		t.Fatalf("raw retention=%d", count(t, s, "buckets"))
	}
	if count(t, s, "rollups") != 61 {
		t.Fatalf("rollups=%d", count(t, s, "rollups"))
	}
}
func TestSQLiteDurationCohortFindsEvictedAttempt(t *testing.T) {
	s, now, _ := testStore(t)
	f := observation.RequestFact{ID: "stable", Provider: "north", Path: "/v1/messages", Outcome: observation.OutcomeFailed, Stage: observation.StageConversionResponse, Started: now.Add(-time.Second), Finished: now, AttemptCount: 100, AttemptsTruncated: true}
	f.AttemptHistogram.Observe(5 * time.Second)
	f.AttemptHistogram.Observe(time.Millisecond)
	commitPacket(t, s, observation.Packet{Requests: []observation.RequestFact{f}})
	lo, hi := int64(2*time.Second), int64(5*time.Second)
	result, err := s.Requests(context.Background(), observation.RequestQuery{Search: "stable", DurationKind: "attempt", DurationMinNS: &lo, DurationMaxNS: &hi, Limit: 1})
	if err != nil || result.Total != 1 || result.Requests[0].ID != "stable" {
		t.Fatalf("cohort=%+v %v", result, err)
	}
}
func TestSQLiteQueueBoundedAndDrainReportsLoss(t *testing.T) {
	s, now, _ := testStore(t)
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if !s.Enqueue(observation.Packet{Intervals: []observation.Interval{sampleInterval(now.Add(-time.Second), 1, time.Second)}}) {
		t.Fatal("first enqueue failed")
	}
	deadline := time.Now().Add(time.Second)
	for s.writer.Stats().WaitCount == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if s.writer.Stats().WaitCount == 0 {
		t.Fatal("writer did not enter blocked transaction")
	}
	for i := range QueueCapacity {
		if !s.Enqueue(observation.Packet{Markers: []observation.Marker{{ID: fmt.Sprintf("marker-%d", i), At: now, Kind: "control", Summary: "paused"}}}) {
			t.Fatalf("queue filled early at %d", i)
		}
	}
	if s.Enqueue(observation.Packet{Requests: []observation.RequestFact{{ID: "dropped"}}, Markers: []observation.Marker{{ID: "lost"}}}) {
		t.Fatal("unbounded queue")
	}
	h := s.Health()
	if h.Pending != 64 || h.DroppedBatches != 1 || h.DroppedFacts != 1 || h.DroppedMarkers != 1 {
		t.Fatalf("health=%+v", h)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestDecodeWorkersCancellationErrorOrder(t *testing.T) {
	start := time.Now()
	v := sampleInterval(start, 1, time.Second)
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]encodedRow, 100)
	for i := range rows {
		rows[i] = encodedRow{data: data}
	}
	rows[3].data = []byte("{broken")
	rows[7].data = []byte("{also-broken")
	if _, err = decodeRows(context.Background(), rows); err == nil || !strings.Contains(err.Error(), "row 3") {
		t.Fatalf("source order error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = decodeRows(ctx, rows); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}
func TestSQLiteBackwardClockDoesNotDuplicateRollups(t *testing.T) {
	s, now, _ := testStore(t)
	v := sampleInterval(now.Add(-2*time.Hour), 1, time.Second)
	commitPacket(t, s, observation.Packet{Intervals: []observation.Interval{v}})
	v2 := sampleInterval(now.Add(-3*time.Hour), 2, time.Second)
	v2.End = v2.Start.Add(-time.Second)
	v2.ClockStep = true
	commitPacket(t, s, observation.Packet{Intervals: []observation.Interval{v2}})
	q := observation.Query{From: now.Add(-4 * time.Hour), To: now, MaxPoints: 4}
	rows, _, truncated, err := s.Read(context.Background(), q, observation.Exclusion{})
	if err != nil || truncated {
		t.Fatal(err)
	}
	var total uint64
	for _, r := range rows {
		total += r.Counts.Terminal
	}
	if total != 2 {
		t.Fatalf("clock step duplicated tiers or dropped a normalized envelope: %+v", rows)
	}
}

func TestSQLiteSingleOwnerPerPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.db")
	first, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close(context.Background()) }()
	_, err = Open(path, Options{})
	if !errors.Is(err, ErrHistoryLocked) {
		t.Fatalf("second owner admitted: %v", err)
	}
	if err = first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("owner not released after close: %v", err)
	}
	if err = second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteFailedOpenDoesNotPoisonPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poison.db")
	if err := os.WriteFile(path, []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("foreign database accepted")
	}
	if _, err := os.Stat(path + ".owner"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owner file left behind after failed open: %v", err)
	}
	// The unrelated file must fail closed on every attempt, not be silently
	// replaced with this application's schema after the first failure.
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("foreign file adopted after a failed open")
	}
}
