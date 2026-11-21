package history

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

type sourceRow struct {
	tier   int
	window int64
	value  observation.Interval
}
type encodedRow struct {
	tier   int
	window int64
	data   []byte
}

func (s *Store) readDB() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reader == nil {
		return nil, ErrClosed
	}
	return s.reader, nil
}
func providerClause(providers []string, args *[]any) string {
	if len(providers) == 0 {
		return ""
	}
	marks := make([]string, len(providers))
	for i, p := range providers {
		marks[i] = "?"
		*args = append(*args, p)
	}
	return " AND provider IN (" + strings.Join(marks, ",") + ")"
}
func (s *Store) Read(ctx context.Context, q observation.Query, exclude observation.Exclusion) ([]observation.Interval, []observation.Marker, bool, error) {
	q, err := observation.ValidateQuery(q)
	if err != nil {
		return nil, nil, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	db, err := s.readDB()
	if err != nil {
		return nil, nil, false, err
	}
	args := []any{q.From.UnixNano(), q.To.UnixNano()}
	p1 := providerClause(q.Providers, &args)
	args = append(args, q.From.UnixNano(), q.To.UnixNano())
	p2 := providerClause(q.Providers, &args)
	sqlText := `SELECT tier,window_ns,payload FROM rollups WHERE max_ns>? AND min_ns<?` + p1 + ` UNION ALL SELECT 1,minute_ns,payload FROM buckets WHERE max_ns>? AND min_ns<?` + p2 + ` ORDER BY 1 DESC,2 ASC LIMIT 100001`
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, nil, false, err
	}
	encoded := make([]encodedRow, 0, 256)
	truncated := false
	bytes := 0
	for rows.Next() {
		var v encodedRow
		if err = rows.Scan(&v.tier, &v.window, &v.data); err != nil {
			_ = rows.Close()
			return nil, nil, false, err
		}
		bytes += len(v.data)
		if len(encoded) >= observation.MaxSourceIntervals || bytes > 64<<20 {
			truncated = true
			break
		}
		encoded = append(encoded, v)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, nil, false, err
	}
	if closeErr != nil {
		return nil, nil, false, closeErr
	}
	decoded, err := decodeRows(ctx, encoded)
	if err != nil {
		return nil, nil, false, err
	}
	selected := selectTiers(q, decoded, exclude)
	markers, err := s.readMarkers(ctx, db, q)
	if err != nil {
		return nil, nil, false, err
	}
	return selected, markers, truncated, nil
}

// decodeRows bounds both worker concurrency and queued jobs. All workers join
// on every return, including cancellation. Earliest source error wins.
func decodeRows(ctx context.Context, rows []encodedRow) ([]sourceRow, error) {
	out := make([]sourceRow, len(rows))
	errs := make([]error, len(rows))
	jobs := make(chan int, 4)
	var wg sync.WaitGroup
	for n := 0; n < min(4, len(rows)); n++ {
		wg.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				out[i].tier = rows[i].tier
				out[i].window = rows[i].window
				errs[i] = json.Unmarshal(rows[i].data, &out[i].value)
				if errs[i] == nil {
					v := &out[i].value
					if v.Epoch == "" || v.Provider == "" || v.Sequence == 0 || v.Start.IsZero() || v.End.IsZero() {
						errs[i] = errors.New("invalid stored history interval")
					}
				}
			}
		})
	}
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, e := range errs {
		if e != nil {
			return nil, fmt.Errorf("history row %d: %w", i, e)
		}
	}
	return out, nil
}
func selectTiers(q observation.Query, rows []sourceRow, exclude observation.Exclusion) []observation.Interval {
	type minuteNode struct {
		parent *sourceRow
		raw    []*sourceRow
	}
	type hourNode struct {
		parent  *sourceRow
		minutes map[int64]*minuteNode
	}
	tree := make(map[string]map[int64]*hourNode)
	for i := range rows {
		row := &rows[i]
		v := &row.value
		if v.LastSequence == 0 {
			v.LastSequence = v.Sequence
		}
		if v.Epoch == exclude.Epoch && exclude.FromSequence > 0 && v.Sequence >= exclude.FromSequence {
			continue
		}
		key := v.Epoch + "\x00" + v.Provider
		hours := tree[key]
		if hours == nil {
			hours = make(map[int64]*hourNode)
			tree[key] = hours
		}
		hour := time.Unix(0, row.window).UTC().Truncate(time.Hour).UnixNano()
		hn := hours[hour]
		if hn == nil {
			hn = &hourNode{minutes: make(map[int64]*minuteNode)}
			hours[hour] = hn
		}
		if row.tier == 3600 {
			hn.parent = row
			continue
		}
		mn := hn.minutes[row.window]
		if mn == nil {
			mn = &minuteNode{}
			hn.minutes[row.window] = mn
		}
		if row.tier == 60 {
			mn.parent = row
		} else {
			mn.raw = append(mn.raw, row)
		}
	}
	preferred := 1
	step := q.To.Sub(q.From) / time.Duration(q.MaxPoints)
	if step >= time.Hour {
		preferred = 3600
	} else if step >= time.Minute {
		preferred = 60
	}
	full := func(r *sourceRow) bool {
		if r == nil {
			return false
		}
		a, b := r.value.Start, r.value.End
		if b.Before(a) {
			a, b = b, a
		}
		return !a.Before(q.From) && !b.After(q.To) && !(r.value.Epoch == exclude.Epoch && exclude.FromSequence > 0 && r.value.LastSequence >= exclude.FromSequence)
	}
	allowed := func(r *sourceRow) bool {
		return r != nil && !(r.value.Epoch == exclude.Epoch && exclude.FromSequence > 0 && r.value.LastSequence >= exclude.FromSequence)
	}
	var selected []*observation.Interval
	for _, hours := range tree {
		for _, hn := range hours {
			if hn.parent != nil && ((preferred >= 3600 && full(hn.parent)) || len(hn.minutes) == 0) {
				if allowed(hn.parent) {
					selected = append(selected, &hn.parent.value)
				}
				continue
			}
			for _, mn := range hn.minutes {
				if mn.parent != nil && ((preferred >= 60 && full(mn.parent)) || len(mn.raw) == 0) {
					if allowed(mn.parent) {
						selected = append(selected, &mn.parent.value)
					}
					continue
				}
				for _, raw := range mn.raw {
					if allowed(raw) {
						selected = append(selected, &raw.value)
					}
				}
			}
		}
	}
	slices.SortFunc(selected, func(a, b *observation.Interval) int {
		as, bs := a.Start, b.Start
		if a.End.Before(as) {
			as = a.End
		}
		if b.End.Before(bs) {
			bs = b.End
		}
		if d := as.Compare(bs); d != 0 {
			return d
		}
		if d := strings.Compare(a.Epoch, b.Epoch); d != 0 {
			return d
		}
		if d := cmp.Compare(a.Sequence, b.Sequence); d != 0 {
			return d
		}
		return strings.Compare(a.Provider, b.Provider)
	})
	out := make([]observation.Interval, len(selected))
	for i, v := range selected {
		out[i] = *v
	}
	return out
}
func (s *Store) readMarkers(ctx context.Context, db *sql.DB, q observation.Query) ([]observation.Marker, error) {
	args := []any{q.From.UnixNano(), q.To.UnixNano()}
	clause := providerClause(q.Providers, &args)
	if clause != "" {
		clause = " AND (provider='' OR " + strings.TrimPrefix(clause, " AND ") + ")"
	}
	rows, err := db.QueryContext(ctx, "SELECT payload FROM markers WHERE at_ns>=? AND at_ns<?"+clause+" ORDER BY at_ns,id LIMIT 1024", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []observation.Marker
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		var m observation.Marker
		if err = json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) Requests(ctx context.Context, q observation.RequestQuery) (observation.RequestResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	q = observation.NormalizeRequestQuery(q)
	if q.LiveOnly {
		return observation.RequestResult{}, nil
	}
	db, err := s.readDB()
	if err != nil {
		return observation.RequestResult{}, err
	}
	where := " WHERE 1=1"
	var args []any
	if !q.From.IsZero() {
		where += " AND at_ns>=?"
		args = append(args, q.From.UnixNano())
	}
	if !q.To.IsZero() {
		where += " AND at_ns<?"
		args = append(args, q.To.UnixNano())
	}
	where += providerClause(q.Providers, &args)
	if q.Outcome != "" {
		where += " AND outcome=?"
		args = append(args, q.Outcome)
	}
	if q.Stage != "" {
		where += " AND stage=?"
		args = append(args, q.Stage)
	}
	if q.Search != "" {
		where += " AND instr(search,?)>0"
		args = append(args, strings.ToLower(q.Search))
	}
	var total int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM requests"+where, args...).Scan(&total); err != nil {
		return observation.RequestResult{}, err
	}
	filterDuration := q.DurationKind != ""
	requested := q
	if filterDuration {
		q.Limit = observation.RequestLimit
		q.Offset = 0
	}
	args = append(args, q.Limit, q.Offset)
	rows, err := db.QueryContext(ctx, "SELECT payload FROM requests"+where+" ORDER BY started_ns DESC,id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return observation.RequestResult{}, err
	}
	defer rows.Close()
	out := observation.RequestResult{Total: total}
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return observation.RequestResult{}, err
		}
		var f observation.RequestFact
		if err = json.Unmarshal(data, &f); err != nil {
			return observation.RequestResult{}, err
		}
		out.Requests = append(out.Requests, f)
	}
	if err = rows.Err(); err != nil {
		return observation.RequestResult{}, err
	}
	if filterDuration {
		return observation.FilterRequests(out.Requests, requested), nil
	}
	out.Truncated = q.Offset+len(out.Requests) < total
	return out, nil
}
