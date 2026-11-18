package requestlog

import (
	"net/http"
	"strings"
	"time"
)

// streamTailNote reports whether the body being logged is an SSE stream. Its tail is
// the last event captured, not the end of a document, so it earns a second note on
// top of the elision marker.
func streamTailNote(h http.Header) bool {
	return strings.HasPrefix(h.Get("Content-Type"), "text/event-stream")
}

// LoggedResponseWriter wraps an http.ResponseWriter, capturing the status code,
// the time to first byte, the byte count and up to BodyLimit() bytes from the head
// and the tail of the body for the body log.
//
// It implements http.Flusher so SSE streaming works transparently.
type LoggedResponseWriter struct {
	http.ResponseWriter
	reqID string
	rec   *Recorder
	start time.Time

	buf         *bodyCapture
	status      int
	wroteHeader bool
	firstByte   time.Time
	bytesOut    int64
}

// NewLoggedResponseWriter creates a wrapper that records the client-visible
// result for the summary and, when body logging is on, the proxy→user entry.
func NewLoggedResponseWriter(w http.ResponseWriter, reqID string, rec *Recorder, start time.Time) *LoggedResponseWriter {
	return &LoggedResponseWriter{
		ResponseWriter: w,
		reqID:          reqID,
		rec:            rec,
		start:          start,
		status:         http.StatusOK,
		buf:            newBodyCapture(BodyLimit()),
	}
}

// WriteHeader captures the status code and forwards to the underlying writer.
func (w *LoggedResponseWriter) WriteHeader(code int) {
	w.status = code
	w.wroteHeader = true
	// Recorded immediately, not in Close: anything that reports on the request
	// before the outermost middleware returns (the latency log line, an error
	// handler) needs to see the real status.
	if w.rec != nil {
		w.rec.Mutate(func(r *Record) {
			// The client-visible status wins: it is what "how many 403s" means.
			r.Status = code
		})
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write buffers the head and tail of the body, then forwards to the underlying
// writer. The middle is counted, not kept.
func (w *LoggedResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.firstByte.IsZero() {
		w.firstByte = time.Now()
	}
	w.buf.Write(b)
	n, err := w.ResponseWriter.Write(b)
	w.bytesOut += int64(n)
	return n, err
}

// Flush delegates to the underlying writer if it supports Flusher (SSE).
func (w *LoggedResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Status returns the HTTP status code that was sent.
func (w *LoggedResponseWriter) Status() int {
	return w.status
}

// Close records the client-visible outcome and, when body logging is on, writes
// the proxy→user entry and closes the request's file.
func (w *LoggedResponseWriter) Close() {
	if w.rec != nil {
		status, bytesOut := w.status, w.bytesOut
		var ttfb int64
		if !w.firstByte.IsZero() {
			ttfb = w.firstByte.Sub(w.start).Milliseconds()
		}
		w.rec.Mutate(func(r *Record) {
			if r.Status == 0 {
				r.Status = status
			}
			r.BytesOut = bytesOut
			if ttfb > 0 {
				r.TTFBMS = ttfb
			}
		})
	}

	logger := Get()
	if logger == nil || !logger.BodiesEnabled() || w.reqID == "" {
		return
	}
	entry := logger.GetEntry(w.reqID)
	if entry == nil {
		return
	}

	respHdrs := headerMap(w.ResponseWriter.Header())
	// The capture already elided the middle of a long body; a stream gets one extra
	// note because its tail is not the end of the conversation, only the end of what
	// was captured.
	body := w.buf.String()
	if streamTailNote(w.ResponseWriter.Header()) && w.buf.Total() > int64(len(body)) {
		body += "\n[... stream continues ...]"
	}

	entry.Log(map[string]interface{}{
		"dir":     "proxy→user",
		"status":  w.status,
		"headers": respHdrs,
		"body":    body,
	})

	logger.CloseEntry(w.reqID)
}
