package requestlog

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// LoggingTransport wraps an http.RoundTripper to log every proxy↔upstream exchange.
// When the global logger is nil (debug disabled), it is a transparent no-op passthrough.
type LoggingTransport struct {
	Base http.RoundTripper
}

// WrapTransport returns a RoundTripper that logs all requests and responses.
// If base is nil, http.DefaultTransport will be used.
func WrapTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &LoggingTransport{Base: base}
}

// RoundTrip executes the request, logging proxy→upstream and upstream→proxy.
func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	logger := Get()
	reqID := RequestIDFromContext(req.Context())

	// Fast path: no debug — passthrough.
	if logger == nil || reqID == "" {
		return t.Base.RoundTrip(req)
	}

	entry := logger.GetEntry(reqID)
	if entry == nil {
		return t.Base.RoundTrip(req)
	}

	// ── proxy→upstream ──
	reqBody := readAndRestoreBody(req)
	reqHdrs := redactHeaders(req.Header)
	entry.Log(map[string]interface{}{
		"dir":    "proxy→upstream",
		"method": req.Method,
		"url":    req.URL.String(),
		"headers": reqHdrs,
		"body":   truncateBody(reqBody, 8<<10),
	})

	// ── Execute ──
	resp, err := t.Base.RoundTrip(req)
	if err != nil {
		entry.Log(map[string]interface{}{
			"dir":   "upstream→proxy",
			"error": err.Error(),
		})
		return nil, err
	}

	// ── upstream→proxy ──
	isStream := isStreamingResponse(resp)
	respBody := readFirstChunk(resp, 8<<10)
	respHdrs := headerMap(resp.Header)

	bodyLog := truncateBody(respBody, 8<<10)
	if isStream {
		bodyLog += "\n[... stream truncated ...]"
	}

	entry.Log(map[string]interface{}{
		"dir":     "upstream→proxy",
		"status":  resp.StatusCode,
		"headers": respHdrs,
		"body":    bodyLog,
	})

	return resp, nil
}

// ── helpers ──

// readAndRestoreBody reads the full request body for logging and restores it.
func readAndRestoreBody(req *http.Request) []byte {
	if req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		req.Body = io.NopCloser(strings.NewReader(""))
		return nil
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

// readFirstChunk reads the first maxBytes from resp.Body, then restores it so
// the caller can still read the full stream (via MultiReader). For non-streaming
// responses reads the whole body.
func readFirstChunk(resp *http.Response, maxBytes int) []byte {
	if resp.Body == nil {
		return nil
	}
	first, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(""))
		return nil
	}
	// Tee: the caller reads first (from the log buffer), then the rest of the stream.
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(first), resp.Body))
	return first
}

// redactHeaders copies headers, redacting sensitive values.
func redactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		key := http.CanonicalHeaderKey(k)
		// Redact bearer tokens and API keys.
		if key == "Authorization" || key == "X-Api-Key" || key == "Api-Key" {
			out[k] = "***REDACTED***"
		} else {
			out[k] = strings.Join(v, ", ")
		}
	}
	return out
}

// headerMap converts http.Header to a flat map.
func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// isStreamingResponse returns true if the response appears to be an SSE stream.
func isStreamingResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "text/event-stream")
}

// truncateBody returns body as a string, truncated with a note if it exceeds limit.
func truncateBody(body []byte, limit int) string {
	if len(body) == 0 {
		return ""
	}
	if len(body) <= limit {
		return string(body)
	}
	return fmt.Sprintf("%s\n[... truncated at %d bytes, total=%d ...]", string(body[:limit]), limit, len(body))
}
