package providers

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression: an upstream that gzips its SSE response must reach the client
// with readable bytes. copyHeaders forwarded the client's Accept-Encoding, so
// Go's transport skipped its own decompression, and the passthrough's
// line-by-line SSE rewrite then corrupted the still-compressed stream —
// clients died with "zlib: Error -3 while decompressing data".
func TestPassthroughDecodesGzippedUpstream(t *testing.T) {
	const sse = "event: message_start\n" +
		"data: {\"type\":\"message_start\"}\n" +
		"\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n" +
		"\n"

	var gotAcceptEncoding []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record what aiproxy actually negotiated.
		gotAcceptEncoding = r.Header.Values("Accept-Encoding")
		// gzip only when the client asked for it, as a real edge does.
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, sse)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, sse)
		_ = zw.Close()
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	p := &ProviderConfig{
		ProviderID: "test",
		Type:       "messages",
		BaseURL:    upstream.URL,
		APIKey:     "test-key",
		AuthHeader: "bearer",
	}
	if err := p.initHTTPClient(); err != nil {
		t.Fatal(err)
	}
	handler := p.makePassthroughHandler("/v1/messages")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"stream":true,"messages":[]}`))
	// The client (Hermes/anthropic SDK) advertises gzip support.
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("passthrough did not return")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("message_stop missing from forwarded body:\n%q", body)
	}
	// The bytes the client receives must be readable, not a gzip blob.
	if !strings.Contains(body, `data: {"type":"message_start"}`) {
		t.Fatalf("SSE payload not readable after proxy (compression leaked?):\n%q", body)
	}
	// Content-Encoding must not be advertised once the body was decoded.
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("Content-Encoding %q forwarded to client with a decoded body", ce)
	}
	// Whatever aiproxy negotiated upstream, it must be able to decode it.
	for _, ae := range gotAcceptEncoding {
		if !strings.Contains(ae, "gzip") {
			t.Fatalf("unexpected upstream Accept-Encoding: %q", ae)
		}
	}
}
