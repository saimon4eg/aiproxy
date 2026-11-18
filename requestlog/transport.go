package requestlog

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LoggingTransport wraps an http.RoundTripper to record every proxy↔upstream
// exchange.
//
// Two duties are separate on purpose:
//   - the summary record (upstream URL, status, stream flag, quota headers and
//     token usage) is fed for every request;
//   - the four body lines are written only when body logging is on.
//
// Body logging used to be the only mode, which meant token counts were
// invisible: it kept the first 8 KB of a response, while a streamed turn reports
// usage at the very end.
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

// RoundTrip executes the request, recording the exchange.
func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	rec := RecorderFromContext(ctx)
	logger := Get()
	reqID := RequestIDFromContext(ctx)

	var entry *Entry
	if logger != nil && logger.BodiesEnabled() && reqID != "" {
		entry = logger.GetEntry(reqID)
	}

	// ── proxy→upstream ──
	if entry != nil {
		reqBody := readAndRestoreBody(req)
		entry.Log(map[string]interface{}{
			"dir":     "proxy→upstream",
			"method":  req.Method,
			"url":     req.URL.String(),
			"headers": redactHeaders(req.Header),
			"body":    ElideBody(reqBody, BodyLimit()),
		})
	}

	Mutate(ctx, func(r *Record) {
		r.UpstreamCalled = true
		r.UpstreamURL = req.URL.String()
	})

	// ── Execute ──
	resp, err := t.Base.RoundTrip(req)
	if err != nil {
		Mutate(ctx, func(r *Record) {
			r.ErrorCode = "upstream_error"
			r.Error = truncateText(err.Error(), 300)
			if r.Status == 0 {
				r.Status = http.StatusBadGateway
			}
		})
		if entry != nil {
			entry.Log(map[string]interface{}{
				"dir":   "upstream→proxy",
				"error": err.Error(),
			})
		}
		return nil, err
	}

	stream := isStreamingResponse(resp)

	// ── upstream→proxy ──
	if entry != nil {
		// Only the head can be captured here: the rest has not arrived yet, and
		// reading it before handing the response back would stall the client. For a
		// stream that wait is charged to the first token, so its head stays small;
		// the tail of a stream is logged on the client side of the proxy, where the
		// body is captured as it flows.
		head := BodyLimit()
		if stream {
			head = streamHeadLimit
		}
		first := readFirstChunk(resp, head)
		bodyLog := ElideBody(first, head)
		if stream {
			bodyLog += "\n[... stream continues ...]"
		}
		entry.Log(map[string]interface{}{
			"dir":     "upstream→proxy",
			"status":  resp.StatusCode,
			"headers": headerMap(resp.Header),
			"body":    bodyLog,
		})
	}

	Mutate(ctx, func(r *Record) {
		r.Status = resp.StatusCode
		r.Stream = r.Stream || stream
		// The upstream's own quota headers are the only signal of how close a
		// subscription is to its window limit.
		if limits := ratelimitHeaders(resp.Header); len(limits) > 0 {
			r.RateLimit = limits
		}
	})

	// Token accounting is read on the way to the client, so it works for streams
	// and with body logging off.
	if rec != nil && resp.Body != nil {
		resp.Body = newMeteredBody(resp.Body, rec)
	}

	return resp, nil
}

// meteredBody scans the response for usage as it streams past.
type meteredBody struct {
	inner   io.ReadCloser
	scanner *UsageScanner
	rec     *Recorder

	once sync.Once
}

func newMeteredBody(inner io.ReadCloser, rec *Recorder) io.ReadCloser {
	return &meteredBody{inner: inner, scanner: NewUsageScanner(), rec: rec}
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if n > 0 {
		b.scanner.Write(p[:n])
	}
	if err == io.EOF {
		b.finish()
	}
	return n, err
}

// Close finalises usage even when the client disconnects mid-stream: whatever
// was generated before that still cost money.
func (b *meteredBody) Close() error {
	b.finish()
	return b.inner.Close()
}

func (b *meteredBody) finish() {
	b.once.Do(func() {
		b.scanner.Finish()
		input, output, cacheRead, cacheWrite := b.scanner.Result()
		if input == 0 && output == 0 && cacheRead == 0 && cacheWrite == 0 {
			return
		}
		b.rec.Mutate(func(r *Record) {
			r.InputTokens = input
			r.OutputTokens = output
			r.CacheReadTokens = cacheRead
			r.CacheWriteTokens = cacheWrite
		})
	})
}

// ratelimitHeaders keeps the upstream's quota headers, which is how a
// subscription's 5h/7d windows can be watched.
func ratelimitHeaders(h http.Header) map[string]string {
	var out map[string]string
	for key, values := range h {
		lower := strings.ToLower(key)
		if !strings.Contains(lower, "ratelimit") && lower != "retry-after" {
			continue
		}
		if out == nil {
			out = make(map[string]string, 8)
		}
		out[lower] = strings.Join(values, ", ")
	}
	return out
}

// ── helpers ──

// streamHeadLimit bounds how much of an SSE stream is read before the response is
// handed back. Anything larger is paid for by the client as delay to its first
// token, so the head stays small and the tail is captured further downstream.
const streamHeadLimit = 4 << 10 // 4 KB

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
// the caller can still read the full stream (via MultiReader).
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
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(first), resp.Body))
	return first
}

// redactHeaders copies headers, redacting sensitive values.
func redactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		key := http.CanonicalHeaderKey(k)
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
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}

// truncateText cuts a message for a log field.
func truncateText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// timestamp helper kept for symmetry with callers that need a monotonic delta.
func sinceMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
