// Package history provides the optional pure-Go SQLite observation sink. The
// request path only enqueues immutable packets; one writer owns all mutations.
package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	_ "modernc.org/sqlite"
)

const ApplicationID = 0x41495348
const SchemaVersion = 1
const QueueCapacity = 64
const ByteCapacity = 64 << 20

type queuedPacket struct {
	packet observation.Packet
	bytes  uint64
}

var ErrForeignDatabase = errors.New("history file belongs to another application or schema")
var ErrConflictingReplay = errors.New("history interval identity was replayed with different data")
var ErrClosed = errors.New("history is closed")

type Options struct {
	Now        func() time.Time // clock for retention and deterministic replay tests
	Retention  time.Duration
	Durability string // normal (WAL NORMAL) or full (WAL FULL).
	ReadOnly   bool
}
type Store struct {
	writer, reader          *sql.DB
	opts                    Options
	mu                      sync.Mutex
	health                  observation.Health
	queue                   chan queuedPacket
	done                    chan struct{}
	closing                 bool
	closeErr                error
	rawBefore, minuteBefore int64
}

// Open accepts an ordinary local filename, never a SQLite URI or network path.
// One running gateway owns a path. Existing unrelated databases fail closed.
func Open(path string, opts Options) (*Store, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Retention == 0 {
		opts.Retention = 168 * time.Hour
	}
	if opts.Retention < time.Hour || opts.Retention > 8760*time.Hour {
		return nil, errors.New("history retention must be between 1h and 8760h")
	}
	opts.Durability = strings.ToLower(opts.Durability)
	if opts.Durability == "" {
		opts.Durability = "normal"
	}
	if opts.Durability != "normal" && opts.Durability != "full" {
		return nil, errors.New("history durability must be normal or full")
	}
	dsn, err := localDSN(path, opts.ReadOnly)
	if err != nil {
		return nil, err
	}
	if !opts.ReadOnly {
		f, e := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if e == nil {
			e = f.Close()
		}
		if e != nil && !errors.Is(e, os.ErrExist) {
			return nil, fmt.Errorf("create history: %w", e)
		}
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(e error) (*Store, error) { _ = db.Close(); return nil, e }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		return fail(err)
	}
	if err = validateSchema(ctx, db, opts.ReadOnly); err != nil {
		return fail(err)
	}
	if !opts.ReadOnly {
		var mode string
		if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
			return fail(err)
		}
		if mode != "wal" {
			return fail(fmt.Errorf("history requires local WAL mode; got %s", mode))
		}
		if _, err = db.ExecContext(ctx, "PRAGMA synchronous="+strings.ToUpper(opts.Durability)); err != nil {
			return fail(err)
		}
	}
	ro, err := localDSN(path, true)
	if err != nil {
		return fail(err)
	}
	reader, err := sql.Open("sqlite", ro)
	if err != nil {
		return fail(err)
	}
	reader.SetMaxOpenConns(2)
	reader.SetMaxIdleConns(2)
	if err = reader.PingContext(ctx); err != nil {
		_ = reader.Close()
		return fail(err)
	}
	s := &Store{writer: db, reader: reader, opts: opts, queue: make(chan queuedPacket, QueueCapacity), done: make(chan struct{}), health: observation.Health{Enabled: true, Healthy: true, Capacity: QueueCapacity, ByteCapacity: ByteCapacity}}
	s.loadMetadata(ctx)
	if opts.ReadOnly {
		s.closing = true
		close(s.done)
	} else {
		go s.run()
	}
	return s, nil
}
func localDSN(path string, readOnly bool) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || strings.HasPrefix(strings.ToLower(path), "file:") || strings.Contains(path, "://") || strings.HasPrefix(path, "//") || strings.HasPrefix(path, `\\`) {
		return "", errors.New("history requires an ordinary local filesystem path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	slash := filepath.ToSlash(abs)
	if len(slash) > 1 && slash[1] == ':' {
		slash = "/" + slash
	}
	u := url.URL{Scheme: "file", Path: slash}
	q := u.Query()
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("mode", "rw")
	}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	return u.String(), nil
}
func validateSchema(ctx context.Context, db *sql.DB, readOnly bool) error {
	var app, version, tables int
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if app == ApplicationID && version == SchemaVersion {
		return nil
	}
	if app != 0 || version != 0 {
		return ErrForeignDatabase
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		return err
	}
	if tables != 0 || readOnly {
		return ErrForeignDatabase
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range schema {
		if _, err = tx.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var schema = []string{
	"PRAGMA application_id=1095324488",
	"PRAGMA user_version=1",
	`CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID`,
	`CREATE TABLE buckets (epoch TEXT NOT NULL, sequence INTEGER NOT NULL, provider TEXT NOT NULL, minute_ns INTEGER NOT NULL, hour_ns INTEGER NOT NULL, min_ns INTEGER NOT NULL, max_ns INTEGER NOT NULL, payload BLOB NOT NULL, PRIMARY KEY(epoch,sequence,provider)) WITHOUT ROWID`,
	`CREATE INDEX buckets_range ON buckets(min_ns,max_ns,provider)`,
	`CREATE INDEX buckets_retention ON buckets(minute_ns)`,
	`CREATE TABLE rollups (tier INTEGER NOT NULL, epoch TEXT NOT NULL, provider TEXT NOT NULL, window_ns INTEGER NOT NULL, first_sequence INTEGER NOT NULL, last_sequence INTEGER NOT NULL, min_ns INTEGER NOT NULL, max_ns INTEGER NOT NULL, payload BLOB NOT NULL, PRIMARY KEY(tier,epoch,provider,window_ns)) WITHOUT ROWID`,
	`CREATE INDEX rollups_range ON rollups(tier,min_ns,max_ns,provider)`,
	`CREATE TABLE receipts (epoch TEXT NOT NULL, sequence INTEGER NOT NULL, provider TEXT NOT NULL, digest BLOB NOT NULL, max_ns INTEGER NOT NULL, PRIMARY KEY(epoch,sequence,provider)) WITHOUT ROWID`,
	`CREATE INDEX receipts_retention ON receipts(max_ns)`,
	`CREATE TABLE requests (id TEXT PRIMARY KEY, provider TEXT NOT NULL, outcome TEXT NOT NULL, stage TEXT NOT NULL, at_ns INTEGER NOT NULL, started_ns INTEGER NOT NULL, search TEXT NOT NULL, payload BLOB NOT NULL) WITHOUT ROWID`,
	`CREATE INDEX requests_range ON requests(at_ns,provider,outcome,stage)`,
	`CREATE INDEX requests_order ON requests(started_ns DESC,id DESC)`,
	`CREATE TABLE markers (id TEXT PRIMARY KEY, at_ns INTEGER NOT NULL, provider TEXT NOT NULL, payload BLOB NOT NULL) WITHOUT ROWID`,
	`CREATE INDEX markers_range ON markers(at_ns,provider)`,
}

func (s *Store) loadMetadata(ctx context.Context) {
	for _, item := range []struct {
		key string
		out *int64
	}{{"raw_before", &s.rawBefore}, {"minute_before", &s.minuteBefore}} {
		_ = s.writer.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", item.key).Scan(item.out)
	}
	var h observation.Health
	var data string
	if err := s.writer.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='health'").Scan(&data); err == nil && json.Unmarshal([]byte(data), &h) == nil {
		s.health.DroppedBatches = h.DroppedBatches
		s.health.DroppedFacts = h.DroppedFacts
		s.health.DroppedMarkers = h.DroppedMarkers
		s.health.WriteErrors = h.WriteErrors
		s.health.CommittedThrough = h.CommittedThrough
	}
}
func (s *Store) Health() observation.Health {
	if s == nil {
		return observation.Health{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.health
	h.Pending = len(s.queue)
	return h
}

// Enqueue transfers ownership. It never waits for I/O or queue space.
func (s *Store) Enqueue(p observation.Packet) bool {
	if s == nil {
		return false
	}
	size := packetBytes(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	if size <= ByteCapacity-s.health.BufferedBytes {
		select {
		case s.queue <- queuedPacket{packet: p, bytes: size}:
			s.health.BufferedBytes += size
			return true
		default:
		}
	}
	s.health.DroppedBatches++
	s.health.DroppedFacts += uint64(len(p.Requests))
	s.health.DroppedMarkers += uint64(len(p.Markers))
	return false
}
func (s *Store) run() {
	defer close(s.done)
	for p := range s.queue {
		batch := []observation.Packet{p.packet}
		bytes := p.bytes
		for len(batch) < QueueCapacity {
			select {
			case p, ok := <-s.queue:
				if !ok {
					goto write
				}
				batch = append(batch, p.packet)
				bytes += p.bytes
			default:
				goto write
			}
		}
	write:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := s.commit(ctx, batch)
		cancel()
		s.mu.Lock()
		s.health.BufferedBytes -= bytes
		if err != nil {
			s.health.Healthy = false
			s.health.WriteErrors++
			s.health.LastError = err.Error()
			if s.closeErr == nil {
				s.closeErr = err
			}
		} else {
			s.health.Healthy = true
			s.health.LastError = ""
		}
		s.mu.Unlock()
	}
}
func (s *Store) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.closing {
		s.closing = true
		close(s.queue)
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reader != nil {
		if err := s.reader.Close(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
		s.reader = nil
	}
	if s.writer != nil {
		if err := s.writer.Close(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
		s.writer = nil
	}
	return s.closeErr
}
