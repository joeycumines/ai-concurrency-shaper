package diagnostic

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"time"
)

// Writer captures bytes actually accepted by the downstream writer, not bytes
// offered by a converter. It forwards ResponseController capabilities and the
// HTTP/1 upgrade path without interpreting upgraded traffic.
type Writer struct {
	dst       http.ResponseWriter
	capture   *Capture
	Status    int
	HeadersAt time.Time
	Err       error
	Hijacked  bool
}

func WrapWriter(dst http.ResponseWriter, c *Capture) *Writer { return &Writer{dst: dst, capture: c} }
func (w *Writer) Header() http.Header                        { return w.dst.Header() }
func (w *Writer) Unwrap() http.ResponseWriter                { return w.dst }
func (w *Writer) WriteHeader(code int) {
	if w.Status != 0 {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.dst.WriteHeader(code)
		return
	}
	w.Status = code
	w.HeadersAt = time.Now()
	w.capture.Metadata(DownstreamResponse, w.Header().Get("Content-Type"), code)
	w.dst.WriteHeader(code)
}
func (w *Writer) Write(p []byte) (int, error) {
	if w.Status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.dst.Write(p)
	if n > 0 {
		w.capture.Append(DownstreamResponse, p[:min(n, len(p))])
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil && w.Err == nil {
		w.Err = err
		w.capture.Failure("downstream", "write", "", "", err.Error())
	}
	return n, err
}
func (w *Writer) FlushError() error {
	if w.Status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	err := http.NewResponseController(w.dst).Flush()
	if err != nil && w.Err == nil {
		w.Err = err
		w.capture.Failure("downstream", "flush", "", "", err.Error())
	}
	return err
}
func (w *Writer) Flush() { _ = w.FlushError() }
func (w *Writer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, b, err := http.NewResponseController(w.dst).Hijack()
	if err == nil {
		w.Hijacked = true
	}
	return c, b, err
}
func (w *Writer) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.dst.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
