package transcode

// Full-flow recorder (diagnostic tap), enabled by the
// -transcode-flowlog-dir command line flag.
//
// When the configured directory is non-empty, every transcoded exchange
// writes one JSON file there capturing the FULL contents of the flow: client
// request (method/path/query/headers/body), converted upstream request
// (method/URL/headers/body), upstream response (status/headers/body),
// downstream response (status/headers/body), the request/response conversion
// reports, the recorded outcome, and timings.
//
// NOTHING IS REDACTED: bodies and headers (including credentials) are stored
// verbatim. Treat the directory as secret-bearing. Bodies that are not valid
// UTF-8 are stored base64 with an explicit flag. Streamed bodies are capped
// (flowBodyCap); over-cap bodies set the truncated flag rather than growing
// memory without bound.
//
// When the directory is empty the tap is fully inert: newFlowCapture
// returns nil and every hook below is a nil-guarded no-op with no measurable
// overhead. This file is diagnostic scaffolding, not product surface.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// flowBodyCap bounds a single captured streamed body. Client and converted
// request bodies are already bounded by the body limits; the cap only bites
// on unbounded upstream/downstream stream bytes.
const flowBodyCap = 64 << 20

var flowSeq atomic.Uint64

// flowBody is one captured message body: verbatim text when valid UTF-8,
// base64 otherwise, with truncation flagged explicitly.
type flowBody struct {
	Text      string `json:"text,omitempty"`
	Base64    string `json:"base64,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Bytes     int    `json:"bytes"`
}

func newFlowBody(data []byte, truncated bool) flowBody {
	return newFlowBodySized(data, len(data), truncated)
}

// newFlowBodySized records total as the size of the source message even when
// only a capped prefix was retained.
func newFlowBodySized(data []byte, total int, truncated bool) flowBody {
	b := flowBody{Truncated: truncated, Bytes: total}
	if utf8.Valid(data) {
		b.Text = string(data)
	} else {
		b.Base64 = base64.StdEncoding.EncodeToString(data)
	}
	return b
}

// flowLoss is the JSON form of one ConversionLoss with a readable kind.
type flowLoss struct {
	Kind    string `json:"kind"`
	Feature string `json:"feature"`
	Path    string `json:"path"`
	Detail  string `json:"detail"`
}

func flowLosses(report ConversionReport) []flowLoss {
	out := make([]flowLoss, 0, len(report.Losses))
	for _, l := range report.Losses {
		kind := "loss"
		if l.Kind == NoteRecord {
			kind = "note"
		}
		out = append(out, flowLoss{
			Kind:    kind,
			Feature: string(l.Feature),
			Path:    l.Path,
			Detail:  l.Detail,
		})
	}
	return out
}

// flowOutcome is the readable form of the exchange Outcome: enum fields are
// rendered as their String() values and presence-aware values are flattened,
// so a record can be read without the Go symbol tables.
type flowOutcome struct {
	UpstreamAttempted  bool   `json:"upstream_attempted"`
	UpstreamStatus     *int   `json:"upstream_status,omitempty"`
	UpstreamFailure    bool   `json:"upstream_failure"`
	RetryAfterMs       *int   `json:"retry_after_ms,omitempty"`
	Provenance         string `json:"provenance"`
	ClientAborted      bool   `json:"client_aborted"`
	DownstreamComplete bool   `json:"downstream_complete"`
	LocalFailure       bool   `json:"local_failure"`
	CircuitRejected    bool   `json:"circuit_rejected"`
	StreamOutcome      string `json:"stream_outcome,omitempty"`
}

// flowOutcomeOf renders outcome for reading. streamClassified selects
// whether the stream classification is meaningful: the zero value of
// streamOutcome is "success", so only an exchange that actually ran the
// streamed response classification may report one — a streaming request
// that failed before the classification (an upstream JSON error, a
// transport error, a converter build failure) must not claim a stream
// outcome.
func flowOutcomeOf(outcome Outcome, streamClassified bool) *flowOutcome {
	out := &flowOutcome{
		UpstreamAttempted:  outcome.UpstreamAttempted,
		UpstreamFailure:    outcome.UpstreamFailure,
		Provenance:         outcome.Provenance.String(),
		ClientAborted:      outcome.ClientAborted,
		DownstreamComplete: outcome.DownstreamComplete,
		LocalFailure:       outcome.LocalFailure,
		CircuitRejected:    outcome.CircuitRejected,
	}
	if streamClassified {
		out.StreamOutcome = outcome.StreamOutcome.String()
	}
	if outcome.UpstreamStatus.Set {
		status := outcome.UpstreamStatus.Value
		out.UpstreamStatus = &status
	}
	if outcome.RetryAfter.Set {
		ms := int(outcome.RetryAfter.Value.Milliseconds())
		out.RetryAfterMs = &ms
	}
	return out
}

// flowRecord is the per-exchange structured output: the full contents of
// every leg of the flow plus the conversion verdicts.
type flowRecord struct {
	ID            string              `json:"id"`
	StartedAt     string              `json:"started_at"`
	DurationMs    int64               `json:"duration_ms"`
	StreamIntent  bool                `json:"stream_intent"`
	ClientMethod  string              `json:"client_method"`
	ClientPath    string              `json:"client_path"`
	ClientQuery   string              `json:"client_query"`
	ClientHeaders map[string][]string `json:"client_headers"`
	ClientBody    flowBody            `json:"client_body"`

	UpstreamMethod  string              `json:"upstream_method,omitempty"`
	UpstreamURL     string              `json:"upstream_url,omitempty"`
	UpstreamHeaders map[string][]string `json:"upstream_headers,omitempty"`
	UpstreamBody    *flowBody           `json:"upstream_body,omitempty"`

	UpstreamStatus      int                 `json:"upstream_status,omitempty"`
	UpstreamRespHeaders map[string][]string `json:"upstream_resp_headers,omitempty"`
	UpstreamRespBody    *flowBody           `json:"upstream_resp_body,omitempty"`

	DownstreamStatus  int                 `json:"downstream_status,omitempty"`
	DownstreamHeaders map[string][]string `json:"downstream_headers,omitempty"`
	DownstreamBody    *flowBody           `json:"downstream_body,omitempty"`

	RequestReport         []flowLoss   `json:"request_report,omitempty"`
	RequestReportDropped  int          `json:"request_report_dropped,omitempty"`
	ResponseReport        []flowLoss   `json:"response_report,omitempty"`
	ResponseReportDropped int          `json:"response_report_dropped,omitempty"`
	Outcome               *flowOutcome `json:"outcome,omitempty"`

	// CommittedStream marks an exchange whose response representation was
	// committed before admission (-queue-comments): the recorded downstream
	// status is the handler's attempted status, while the client may have
	// seen the committed one.
	CommittedStream bool `json:"committed_stream,omitempty"`
}

// cappedBuffer accumulates tee'd stream bytes up to a cap; beyond the cap it
// discards and flags truncation. Write never fails so TeeReader flows on.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	cap       int
	truncated bool
	total     int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += len(p)
	if len(p) == 0 {
		return 0, nil
	}
	room := c.cap - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) snapshot() flowBody {
	c.mu.Lock()
	defer c.mu.Unlock()
	return newFlowBodySized(append([]byte(nil), c.buf.Bytes()...), c.total, c.truncated)
}

// teeReadCloser tees an upstream response body into a capped buffer.
type teeReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func (t teeReadCloser) Read(p []byte) (int, error) { return t.reader.Read(p) }
func (t teeReadCloser) Close() error               { return t.closer.Close() }

// teeResponseWriter tees downstream bytes/status/headers. It implements
// http.Flusher, and Unwrap so http.ResponseController reaches the wrapped
// writer's own flush path (the proxy's statusRecorder implements
// FlushError + Unwrap, not http.Flusher): flushing through the tee must
// preserve per-frame delivery and flush-failure accounting exactly as it
// behaves without the recorder. Transcoded routes never hijack (Upgrade is
// rejected), so http.Hijacker is deliberately not forwarded.
type teeResponseWriter struct {
	w           http.ResponseWriter
	buf         *cappedBuffer
	mu          sync.Mutex
	status      int
	wroteHeader bool
	headers     http.Header
}

// Unwrap exposes the wrapped writer to http.ResponseController, so its
// FlushError/Unwrap chain is used instead of this wrapper's Flush.
func (t *teeResponseWriter) Unwrap() http.ResponseWriter { return t.w }

// FlushError delegates to the wrapped writer's own flush path. It must be
// the Error form: http.ResponseController prefers FlushError, and the
// proxy's statusRecorder records flush failures there, so swallowing the
// error here would silently drop flush-failure accounting while the
// recorder is enabled.
func (t *teeResponseWriter) FlushError() error {
	return http.NewResponseController(t.w).Flush()
}

func (t *teeResponseWriter) Header() http.Header { return t.w.Header() }

func (t *teeResponseWriter) WriteHeader(status int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.wroteHeader {
		t.wroteHeader = true
		t.status = status
		t.headers = t.w.Header().Clone()
	}
	t.w.WriteHeader(status)
}

func (t *teeResponseWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	if !t.wroteHeader {
		t.wroteHeader = true
		t.status = http.StatusOK
		t.headers = t.w.Header().Clone()
	}
	t.mu.Unlock()
	// Delegate first and record only the bytes the wrapped writer accepted:
	// on a failed or short write the record must not overstate what the
	// client actually received.
	n, err := t.w.Write(p)
	if n > 0 {
		t.buf.Write(p[:n])
	}
	return n, err
}

func (t *teeResponseWriter) Flush() {
	t.mu.Lock()
	if !t.wroteHeader {
		t.wroteHeader = true
		t.status = http.StatusOK
		t.headers = t.w.Header().Clone()
	}
	t.mu.Unlock()
	// Delegate through http.ResponseController: a wrapped statusRecorder
	// implements FlushError + Unwrap (not http.Flusher), and its flush
	// failure accounting must stay in effect while the recorder is on.
	_ = t.FlushError()
}

func (t *teeResponseWriter) snapshot() (int, http.Header, flowBody) {
	t.mu.Lock()
	defer t.mu.Unlock()
	status := t.status
	if !t.wroteHeader {
		status = 0
	}
	return status, t.headers, t.buf.snapshot()
}

// flowCapture accumulates one exchange. All methods are nil-safe: a nil
// receiver (tap disabled) is a no-op.
type flowCapture struct {
	dir   string
	seq   uint64
	start time.Time

	mu               sync.Mutex
	method           string
	path             string
	query            string
	clientHeaders    http.Header
	clientBody       []byte
	streamIntent     bool
	upMethod         string
	upURL            string
	upHeaders        http.Header
	upBody           []byte
	upBodySet        bool
	upStatus         int
	upRespHeaders    http.Header
	upRespBuf        *cappedBuffer
	downTee          *teeResponseWriter
	requestReport    ConversionReport
	responseReport   ConversionReport
	haveReqReport    bool
	haveRespReport   bool
	outcome          Outcome
	haveOutcome      bool
	committed        bool
	streamClassified bool
	sealOnce         sync.Once
}

type flowContextKey struct{}

// flowSlug maps a client-controlled request identifier (method, path) to a
// filesystem-safe token: characters outside the allow-list become '_', and
// the result is bounded. An empty result becomes "root".
func flowSlug(value string) string {
	const maxSlug = 120
	var b strings.Builder
	for i, r := range value {
		if i >= maxSlug {
			break
		}
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	slug := strings.Trim(b.String(), "._")
	if slug == "" {
		return "root"
	}
	return slug
}

// writeFlowRecord writes one record with O_EXCL, retrying with a numeric
// suffix when the name is taken (pid reuse across separate runs sharing a
// directory must not overwrite an existing record).
func writeFlowRecord(dir, name string, data []byte) {
	for attempt := range 100 {
		candidate := name
		if attempt > 0 {
			base := strings.TrimSuffix(name, ".json")
			candidate = fmt.Sprintf("%s.%d.json", base, attempt)
		}
		f, err := os.OpenFile(filepath.Join(dir, candidate), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			log.Printf("transcode: flow recorder: write record %q: %v", candidate, err)
			return
		}
		if _, err := f.Write(data); err != nil {
			log.Printf("transcode: flow recorder: write record %q: %v", candidate, err)
		}
		if err := f.Close(); err != nil {
			log.Printf("transcode: flow recorder: close record %q: %v", candidate, err)
		}
		return
	}
	log.Printf("transcode: flow recorder: no free record name for %q", name)
}

// flowPathSlug maps a request path to a filename token bounded well under
// any platform's NAME_MAX: a readable prefix, plus a stable hash suffix when
// the sanitized slug exceeds the prefix budget. The slug is lossy for
// non-ASCII paths (every such rune becomes '_'), so two paths can share a
// token; the record's client_path always carries the true path, and the
// O_EXCL retry in writeFlowRecord guarantees one file per exchange
// regardless.
func flowPathSlug(path string) string {
	slug := flowSlug(path)
	const maxPrefix = 72
	if len(slug) <= maxPrefix {
		return slug
	}
	sum := fnv.New64a()
	_, _ = sum.Write([]byte(path))
	return fmt.Sprintf("%s-%016x", truncateUTF8(slug, maxPrefix), sum.Sum64())
}

// truncateUTF8 returns the longest prefix of s no longer than limit bytes
// that ends on a rune boundary, so a multi-byte rune is never split into an
// invalid fragment.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// newFlowCapture starts one exchange capture when the tap is enabled (the
// configured directory is non-empty), else nil.
func newFlowCapture(dir string) *flowCapture {
	if dir == "" {
		return nil
	}
	return &flowCapture{
		dir:   dir,
		seq:   flowSeq.Add(1),
		start: time.Now(),
	}
}

func withFlowCapture(ctx context.Context, fl *flowCapture) context.Context {
	return context.WithValue(ctx, flowContextKey{}, fl)
}

func flowFromContext(ctx context.Context) *flowCapture {
	if fl, ok := ctx.Value(flowContextKey{}).(*flowCapture); ok {
		return fl
	}
	return nil
}

// wrapWriter tees downstream bytes. Nil-safe: returns w unchanged when
// disabled.
func (fl *flowCapture) wrapWriter(w http.ResponseWriter) http.ResponseWriter {
	if fl == nil {
		return w
	}
	tee := &teeResponseWriter{w: w, buf: &cappedBuffer{cap: flowBodyCap}}
	fl.mu.Lock()
	fl.downTee = tee
	fl.mu.Unlock()
	return tee
}

func (fl *flowCapture) setClientRequest(r *http.Request) {
	if fl == nil {
		return
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.method = r.Method
	fl.path = r.URL.Path
	fl.query = r.URL.RawQuery
	fl.clientHeaders = r.Header.Clone()
	// The resolved stream intent is only known after decoding; until then
	// the Accept header is the best available signal, so an exchange
	// rejected before conversion still records the client's intent.
	fl.streamIntent = AcceptIsEventStream(r.Header.Get("Accept"))
}

// setClientBody records the client body once it has been read; an early
// rejection (413/400 before the read completes) still keeps the request
// identity captured by setClientRequest.
func (fl *flowCapture) setClientBody(body []byte) {
	if fl == nil {
		return
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.clientBody = append([]byte(nil), body...)
}

// setStreamClassified records that the exchange reached the streamed
// response classification: only then is the recorded StreamOutcome
// meaningful (its zero value is a success).
func (fl *flowCapture) setStreamClassified() {
	if fl == nil {
		return
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.streamClassified = true
}

// setCommitted records whether the response representation was committed
// before admission (-queue-comments).
func (fl *flowCapture) setCommitted(committed bool) {
	if fl == nil {
		return
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.committed = committed
}

func (fl *flowCapture) setUpstreamRequest(body []byte, streamIntent bool) {
	if fl == nil {
		return
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.upBody = append([]byte(nil), body...)
	fl.upBodySet = true
	fl.streamIntent = streamIntent
}

func (fl *flowCapture) setUpstreamTarget(req *http.Request) {
	if fl == nil {
		return
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.upMethod = req.Method
	fl.upURL = req.URL.String()
	fl.upHeaders = req.Header.Clone()
}

// wrapUpstreamBody tees the upstream response bytes (status/headers too).
// Nil-safe: returns body unchanged when disabled.
func (fl *flowCapture) wrapUpstreamBody(body io.ReadCloser, resp *http.Response) io.ReadCloser {
	if fl == nil {
		return body
	}
	buf := &cappedBuffer{cap: flowBodyCap}
	fl.mu.Lock()
	fl.upStatus = resp.StatusCode
	fl.upRespHeaders = resp.Header.Clone()
	fl.upRespBuf = buf
	fl.mu.Unlock()
	return teeReadCloser{reader: io.TeeReader(body, buf), closer: body}
}

func (fl *flowCapture) addReport(stage string, report ConversionReport) {
	if fl == nil {
		return
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if stage == "request" {
		fl.requestReport = report
		fl.haveReqReport = true
	} else {
		fl.responseReport = report
		fl.haveRespReport = true
	}
}

// setOutcome records the FIRST outcome of the exchange, matching the
// OutcomeSink's once-only semantics: recordOutcome can run more than once
// (the handler's default local-failure defer), and the record must not
// disagree with the journal/metrics about which outcome was authoritative.
func (fl *flowCapture) setOutcome(outcome Outcome) {
	if fl == nil {
		return
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.haveOutcome {
		return
	}
	fl.outcome = outcome
	fl.haveOutcome = true
}

// seal writes the single per-exchange JSON file. Safe to call repeatedly;
// only the first call writes.
func (fl *flowCapture) seal() {
	if fl == nil {
		return
	}
	fl.sealOnce.Do(func() {
		fl.mu.Lock()
		defer fl.mu.Unlock()
		// The sequence is per-process, so the pid is part of the name: two
		// processes sharing one directory must not overwrite each other's
		// records. The method and path are client-controlled, so both are
		// sanitized to an allow-listed slug: a path separator (either
		// platform's) must never select a file outside the directory.
		name := fmt.Sprintf(
			"%06d-%d-%s-%s.json",
			fl.seq,
			os.Getpid(),
			flowSlug(fl.method),
			flowPathSlug(fl.path),
		)
		rec := flowRecord{
			ID:                  fmt.Sprintf("%06d", fl.seq),
			StartedAt:           fl.start.UTC().Format(time.RFC3339Nano),
			DurationMs:          time.Since(fl.start).Milliseconds(),
			StreamIntent:        fl.streamIntent,
			ClientMethod:        fl.method,
			ClientPath:          fl.path,
			ClientQuery:         fl.query,
			ClientHeaders:       map[string][]string(fl.clientHeaders),
			ClientBody:          newFlowBody(fl.clientBody, false),
			UpstreamMethod:      fl.upMethod,
			UpstreamURL:         fl.upURL,
			UpstreamHeaders:     map[string][]string(fl.upHeaders),
			UpstreamStatus:      fl.upStatus,
			UpstreamRespHeaders: map[string][]string(fl.upRespHeaders),
		}
		if fl.upBodySet {
			upBody := newFlowBody(fl.upBody, false)
			rec.UpstreamBody = &upBody
		}
		if fl.upRespBuf != nil {
			b := fl.upRespBuf.snapshot()
			rec.UpstreamRespBody = &b
		}
		if fl.downTee != nil {
			status, headers, body := fl.downTee.snapshot()
			rec.DownstreamStatus = status
			rec.DownstreamHeaders = map[string][]string(headers)
			rec.DownstreamBody = &body
		}
		if fl.haveReqReport {
			rec.RequestReport = flowLosses(fl.requestReport)
			rec.RequestReportDropped = fl.requestReport.Dropped
		}
		if fl.haveRespReport {
			rec.ResponseReport = flowLosses(fl.responseReport)
			rec.ResponseReportDropped = fl.responseReport.Dropped
		}
		if fl.haveOutcome {
			rec.Outcome = flowOutcomeOf(fl.outcome, fl.streamClassified)
		}
		rec.CommittedStream = fl.committed
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			log.Printf("transcode: flow recorder: marshal record: %v", err)
			return
		}
		writeFlowRecord(fl.dir, name, data)
	})
}
