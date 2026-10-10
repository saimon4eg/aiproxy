package providers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression: the native passthrough must forward the complete Anthropic
// terminal event — "event: message_stop", its "data:" line and the blank
// terminator — before closing. Breaking on the event line alone truncated the
// event, and clients that require message_stop (Hermes) failed mid-stream.
func TestPassthroughForwardsCompleteMessageStopEvent(t *testing.T) {
	const upstreamBody = "event: message_start\n" +
		"data: {\"type\":\"message_start\"}\n" +
		"\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n" +
		"\n" +
		": keepalive-after-terminal-event\n"

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstreamBody)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Hold the connection open after the terminal event: the passthrough
		// must return on the SSE terminator, not wait for EOF.
		<-release
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	p := &ProviderConfig{
		ProviderID: "test",
		Type:       "messages",
		BaseURL:    upstream.URL,
		APIKey:     "test-key",
		AuthHeader: "bearer",
	}
	p.initHTTPClient()
	handler := p.makePassthroughHandler("/v1/messages")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true,"messages":[]}`))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough did not return after the message_stop terminator")
	}

	got := rec.Body.String()
	if !strings.Contains(got, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
		t.Fatalf("message_stop event was truncated; forwarded body:\n%q", got)
	}
	if strings.Contains(got, "keepalive-after-terminal-event") {
		t.Fatalf("passthrough forwarded lines past the message_stop terminator:\n%q", got)
	}
}
