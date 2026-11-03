package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
)

func isContextCancellationError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// nowUnix returns the current unix time in seconds as a float64: the
// Responses contract's created_at is float64, so the generated stream
// identity timestamps share the type end-to-end.

func nowUnix() float64 {
	return float64(time.Now().Unix())
}

// derefInt returns the value of v, or 0 when v is nil.

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// logConversionReport logs approved losses and notes for observability. It
// aggregates ALL entries of one conversion into a SINGLE line for the given
// stage (request/response), so a route whose defaults approve several losses
// does not flood the log/TUI with one line per loss. Each entry is classified:
// an approved loss (the policy allows it) is logged as `feature at path`; a
// Note (a sanctioned encoding recorded WITHOUT a policy decision, losses.go
// Note) is logged as `note: feature at path: <detail>`, because the startup
// summary only covers the policy's Allowed set. The header is `approved
// loss(es)` when every entry is an approved loss, else `loss(es)/note(s)`.
// Duplicate feature@path entries are deduped preserving first-seen order.

func (h *TranscodeHandler) logConversionReport(report ConversionReport, r *http.Request, stage string) {
	flowFromContext(r.Context()).addReport(stage, report)
	if len(report.Losses) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(report.Losses))
	var entries []string
	anyNote := false
	for _, loss := range report.Losses {
		key := fmt.Sprintf("%s at %s", loss.Feature, loss.Path)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if loss.Kind == LossRecord {
			entries = append(entries, key)
		} else {
			anyNote = true
			if loss.Detail != "" {
				entries = append(entries, fmt.Sprintf("note: %s: %s", key, loss.Detail))
			} else {
				entries = append(entries, fmt.Sprintf("note: %s", key))
			}
		}
	}
	if len(entries) == 0 {
		return
	}
	if report.Dropped > 0 {
		entries = append(entries, fmt.Sprintf(
			"note: report_overflow: %d further entries dropped after the bound (the report-overflow bound)",
			report.Dropped,
		))
	}
	header := "approved loss(es)"
	if anyNote {
		header = "loss(es)/note(s)"
	}
	log.Printf(
		"transcode: %s %s: %s %s: %s",
		r.Method,
		r.URL.Path,
		stage,
		header,
		strings.Join(entries, ", "),
	)
}

// boundErrorMessage truncates a client-visible error message to the
// configured ErrorMessageBytes bound. It delegates to the shared
// BoundErrorMessage so the bound policy has exactly one home.

func (h *TranscodeHandler) boundErrorMessage(message string) string {
	return BoundErrorMessage(message, h.cfg.BodyLimits.ErrorMessageBytes)
}

// logSafeText makes text safe to place in ONE operator log line. Every byte
// a client can influence must be neutralized: error text routinely embeds raw
// client JSON (encoding/json puts the offending KEY into UnmarshalTypeError,
// and a decoded key may contain a newline), and the request target is bounded
// only by net/http's header limit. Without this, a single request could
// inject a newline and forge a fully-formed "transcode: ..." operator line -
// which the TUI log ring would then render as its own entry. Control
// characters are escaped rather than dropped so the evidence survives, and the
// result is length-bounded so one request cannot flood the log.

func logSafeText(s string) string {
	const maxLoggedField = 512
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x`)
			const hex = "0123456789abcdef"
			b.WriteByte(hex[(r>>4)&0xf])
			b.WriteByte(hex[r&0xf])
		default:
			b.WriteRune(r)
		}
		if b.Len() >= maxLoggedField {
			b.WriteString("…")
			return b.String()
		}
	}
	return b.String()
}

// logRequestError logs a local failure with its detail (never the client
// message): local construction and conversion failures are logged,
// sanitized, and reported neutrally (the conversion-failure path stays
// observable to the operator).

func (h *TranscodeHandler) logRequestError(r *http.Request, err error) {
	log.Printf("transcode: %s %s: %v",
		logSafeText(r.Method), logSafeText(r.URL.Path), logSafeText(err.Error()))
}

// logStreamConversionError records the operator-visible reason a live
// translated stream failed locally: the client gets the dialect error event,
// but without this line the log shows nothing. A stream that ran out without
// a terminal has no converter error, only that fact.

func (h *TranscodeHandler) logStreamConversionError(r *http.Request, cause error) {
	detail := "the upstream stream ended before a terminal event and no local error event was written"
	if cause != nil && !errors.Is(cause, io.EOF) {
		detail = h.boundErrorMessage(cause.Error())
	}
	h.logRequestError(r, fmt.Errorf("[%s] convert stream response: %s", ProvenanceLocalResponseConversionError, detail))
}

// sanitizeUpstreamTransportError redacts credential-bearing URL query values
// from an upstream transport error before it is logged, with the same
// guarantees as the native path (auth.SanitizeURLError).

func sanitizeUpstreamTransportError(err error) error {
	return auth.SanitizeURLError(err)
}
