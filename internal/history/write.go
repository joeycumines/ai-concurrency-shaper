package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

type rollupKey struct {
	tier            int
	epoch, provider string
	window          int64
}

func envelope(v observation.Interval) (time.Time, time.Time) {
	if v.End.Before(v.Start) {
		return v.End, v.Start
	}
	return v.Start, v.End
}
func (s *Store) commit(ctx context.Context, packets []observation.Packet) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.opts.Now()
	retention := now.Add(-s.opts.Retention).UnixNano()
	rawBefore := max(s.rawBefore, now.Add(-15*time.Minute).UTC().Truncate(time.Minute).UnixNano())
	updates := make(map[rollupKey]observation.Interval)
	latest := time.Time{}
	for _, packet := range packets {
		for _, v := range packet.Intervals {
			if v.Epoch == "" || v.Provider == "" || v.Sequence == 0 || v.Start.IsZero() || v.End.IsZero() || len(v.Resources) > observation.MaxResourcePools {
				return errors.New("invalid history interval")
			}
			if v.LastSequence == 0 {
				v.LastSequence = v.Sequence
			}
			if v.LastSequence != v.Sequence {
				return errors.New("only absolute raw intervals can be enqueued")
			}
			data, e := json.Marshal(v)
			if e != nil {
				return e
			}
			digest := sha256.Sum256(data)
			a, b := envelope(v)
			receipt, e := tx.ExecContext(ctx, "INSERT INTO receipts(epoch,sequence,provider,digest,max_ns) VALUES(?,?,?,?,?) ON CONFLICT(epoch,sequence,provider) DO NOTHING", v.Epoch, v.Sequence, v.Provider, digest[:], b.UnixNano())
			if e != nil {
				return e
			}
			inserted, e := receipt.RowsAffected()
			if e != nil {
				return e
			}
			if inserted == 0 {
				var previous []byte
				if e = tx.QueryRowContext(ctx, "SELECT digest FROM receipts WHERE epoch=? AND sequence=? AND provider=?", v.Epoch, v.Sequence, v.Provider).Scan(&previous); e != nil {
					return e
				}
				if !slices.Equal(previous, digest[:]) {
					return ErrConflictingReplay
				}
				continue
			}
			minute := a.UTC().Truncate(time.Minute).UnixNano()
			hour := a.UTC().Truncate(time.Hour).UnixNano()
			if minute >= rawBefore {
				if _, e = tx.ExecContext(ctx, `INSERT INTO buckets(epoch,sequence,provider,minute_ns,hour_ns,min_ns,max_ns,payload) VALUES(?,?,?,?,?,?,?,?)`, v.Epoch, v.Sequence, v.Provider, minute, hour, a.UnixNano(), b.UnixNano(), data); e != nil {
					return e
				}
			}
			for _, tier := range []int{60, 3600} {
				window := minute
				if tier == 3600 {
					window = hour
				}
				k := rollupKey{tier, v.Epoch, v.Provider, window}
				agg, ok := updates[k]
				if !ok {
					var raw []byte
					e = tx.QueryRowContext(ctx, "SELECT payload FROM rollups WHERE tier=? AND epoch=? AND provider=? AND window_ns=?", tier, v.Epoch, v.Provider, window).Scan(&raw)
					if e == nil {
						if e = json.Unmarshal(raw, &agg); e != nil {
							return fmt.Errorf("decode stored rollup: %w", e)
						}
						ok = true
					} else if !errors.Is(e, sql.ErrNoRows) {
						return e
					}
				}
				if !ok {
					agg = v
					agg.Resources = append([]observation.ResourceIntegral(nil), v.Resources...)
				} else {
					oldStart, oldEnd := envelope(agg)
					// Only the two adjacent endpoint brackets matter for a
					// boundary check; an accumulated sum would hide gaps.
					epsilon := time.Duration(max(agg.ClockUncertaintyNS, v.ClockUncertaintyNS))
					if a.After(oldEnd.Add(epsilon)) || b.Before(oldStart.Add(-epsilon)) {
						agg.Gap = true
					}
					if a.Before(oldEnd.Add(-epsilon)) && b.After(oldStart.Add(epsilon)) {
						agg.Overlap = true
					}
					first := min(agg.Sequence, v.Sequence)
					agg.Add(v)
					agg.Sequence = first
				}
				updates[k] = agg
			}
			if b.After(latest) {
				latest = b
			}
		}
		for _, f := range packet.Requests {
			data, e := json.Marshal(f)
			if e != nil {
				return e
			}
			if f.ID == "" || len(f.Attempts) > observation.AttemptDetailLimit {
				return errors.New("invalid request fact")
			}
			at := f.Finished
			if at.IsZero() {
				at = f.Started
			}
			search := strings.ToLower(strings.Join([]string{f.ID, f.Provider, f.Method, f.Path, f.Route, f.Model, f.ClientProtocol, f.UpstreamProtocol, f.Fingerprint, string(f.Outcome), string(f.Stage)}, " "))
			_, e = tx.ExecContext(ctx, `INSERT INTO requests(id,provider,outcome,stage,at_ns,started_ns,search,payload) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET provider=excluded.provider,outcome=excluded.outcome,stage=excluded.stage,at_ns=excluded.at_ns,started_ns=excluded.started_ns,search=excluded.search,payload=excluded.payload`, f.ID, f.Provider, f.Outcome, f.Stage, at.UnixNano(), f.Started.UnixNano(), search, data)
			if e != nil {
				return e
			}
		}
		for _, m := range packet.Markers {
			data, e := json.Marshal(m)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO markers(id,at_ns,provider,payload) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING", m.ID, m.At.UnixNano(), m.Provider, data); e != nil {
				return e
			}
		}
	}
	for k, v := range updates {
		if _, expired := envelope(v); expired.Before(time.Unix(0, retention)) {
			continue
		}
		data, e := json.Marshal(v)
		if e != nil {
			return e
		}
		a, b := envelope(v)
		_, e = tx.ExecContext(ctx, `INSERT INTO rollups(tier,epoch,provider,window_ns,first_sequence,last_sequence,min_ns,max_ns,payload) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(tier,epoch,provider,window_ns) DO UPDATE SET first_sequence=excluded.first_sequence,last_sequence=excluded.last_sequence,min_ns=excluded.min_ns,max_ns=excluded.max_ns,payload=excluded.payload`, k.tier, k.epoch, k.provider, k.window, v.Sequence, v.LastSequence, a.UnixNano(), b.UnixNano(), data)
		if e != nil {
			return e
		}
	}
	// Cutoffs only move forward. A backward wall-clock step cannot reconstruct an
	// expired rollup from a now-incomplete set of raw children.
	minuteBefore := max(s.minuteBefore, now.Add(-24*time.Hour).UTC().Truncate(time.Hour).UnixNano())
	// Rollups must not outlive the configured retention. Receipts are kept
	// through the end of the last surviving hourly window so a replay into
	// that rollup cannot double-count after its receipt was expired.
	alignedReceiptBefore := now.Add(-s.opts.Retention).UTC().Truncate(time.Hour).UnixNano()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM buckets WHERE minute_ns < ?", []any{rawBefore}},
		{"DELETE FROM rollups WHERE tier=60 AND (window_ns < ? OR max_ns < ?)", []any{minuteBefore, retention}},
		{"DELETE FROM rollups WHERE tier=3600 AND max_ns < ?", []any{retention}},
		{"DELETE FROM receipts WHERE max_ns < ?", []any{alignedReceiptBefore}},
		{"DELETE FROM requests WHERE at_ns < ?", []any{retention}},
		{"DELETE FROM requests WHERE id IN (SELECT id FROM requests ORDER BY started_ns DESC,id DESC LIMIT -1 OFFSET 10000)", nil},
		{"DELETE FROM markers WHERE at_ns < ?", []any{retention}},
		{"DELETE FROM markers WHERE id IN (SELECT id FROM markers ORDER BY at_ns DESC,id DESC LIMIT -1 OFFSET 10000)", nil},
	} {
		if _, err = tx.ExecContext(ctx, q.sql, q.args...); err != nil {
			return err
		}
	}
	s.mu.Lock()
	health := s.health
	s.mu.Unlock()
	var previousHealth observation.Health
	var previousJSON string
	e := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='health'").Scan(&previousJSON)
	if e == nil {
		if e = json.Unmarshal([]byte(previousJSON), &previousHealth); e != nil {
			return e
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if health.DroppedBatches > previousHealth.DroppedBatches || health.WriteErrors > previousHealth.WriteErrors {
		marker := observation.Marker{ID: fmt.Sprintf("recorder-%d-%d-%d", now.UnixNano(), health.DroppedBatches, health.WriteErrors), At: now, Kind: "recorder_gap", Summary: fmt.Sprintf("History omitted %d queued packets; %d writer transactions failed. Charts show missing coverage.", health.DroppedBatches-previousHealth.DroppedBatches, health.WriteErrors-previousHealth.WriteErrors)}
		raw, e := json.Marshal(marker)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO markers(id,at_ns,provider,payload) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING", marker.ID, marker.At.UnixNano(), "", raw); e != nil {
			return e
		}
	}

	if latest.After(health.CommittedThrough) {
		health.CommittedThrough = latest
	}
	data, err := json.Marshal(health)
	if err != nil {
		return err
	}
	for key, value := range map[string]string{"raw_before": strconv.FormatInt(rawBefore, 10), "minute_before": strconv.FormatInt(minuteBefore, 10), "health": string(data)} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	s.rawBefore = rawBefore
	s.minuteBefore = minuteBefore
	if latest.After(s.health.CommittedThrough) {
		s.health.CommittedThrough = latest
	}
	s.mu.Unlock()
	return nil
}
