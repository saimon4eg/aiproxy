package requestlog

import (
	"bytes"
	"fmt"
	"net/http"
)

// maxRespLog is the maximum body size captured for the proxy→user log entry.
const maxRespLog = 8 << 10 // 8KB

// LoggedResponseWriter wraps an http.ResponseWriter, capturing the status code
// and first maxRespLog bytes of the body for logging in Close().
// Implements http.Flusher so SSE streaming works transparently.
type LoggedResponseWriter struct {
	http.ResponseWriter
	reqID       string
	buf         bytes.Buffer
	status      int
	wroteHeader bool
}

// NewLoggedResponseWriter creates a wrapper that logs proxy→user on Close().
func NewLoggedResponseWriter(w http.ResponseWriter, reqID string) *LoggedResponseWriter {
	return &LoggedResponseWriter{
		ResponseWriter: w,
		reqID:          reqID,
		status:         http.StatusOK,
	}
}

// WriteHeader captures the status code and forwards to the underlying writer.
func (w *LoggedResponseWriter) WriteHeader(code int) {
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

// Write buffers the first maxRespLog bytes, then forwards to the underlying writer.
func (w *LoggedResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	// Buffer up to maxRespLog bytes.
	if w.buf.Len() < maxRespLog {
		remaining := maxRespLog - w.buf.Len()
		if len(b) <= remaining {
			w.buf.Write(b)
		} else {
			w.buf.Write(b[:remaining])
		}
	}
	return w.ResponseWriter.Write(b)
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

// Close writes the proxy→user log entry and closes the log file for this request.
// Must be called after the handler has finished writing the response.
func (w *LoggedResponseWriter) Close() {
	logger := Get()
	if logger == nil || w.reqID == "" {
		return
	}

	entry := logger.GetEntry(w.reqID)
	if entry == nil {
		return
	}

	// Collect response headers (readable from ResponseWriter at any point).
	respHdrs := headerMap(w.ResponseWriter.Header())

	body := w.buf.String()
	if w.buf.Len() >= maxRespLog {
		body += fmt.Sprintf("\n[... truncated at %d bytes ...]", maxRespLog)
	}

	entry.Log(map[string]interface{}{
		"dir":     "proxy→user",
		"status":  w.status,
		"headers": respHdrs,
		"body":    body,
	})

	// Log is complete — close and clean up.
	logger.CloseEntry(w.reqID)
}
