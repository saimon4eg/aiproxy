package providers

// Token accounting for the claude-cli transport.
//
// The CLI is a separate process: it dials the relay with its own context, so the
// incoming request's record does not reach that hop by itself. Before the record
// was attached explicitly, a turn that really reached Anthropic was logged with
// upstream_called=false and zero tokens — the subscription's usage was invisible
// in /admin/usage and the summary lied about the upstream call.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whtsky/copilot2api/requestlog"
)

// claudeStreamSSE is a minimal but complete Anthropic stream: usage arrives in
// the terminal message_delta, which is exactly the part a prefix-only logger
// could never see.
const claudeStreamSSE = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":137,\"output_tokens\":42,\"cache_read_input_tokens\":900,\"cache_creation_input_tokens\":11}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

// newAccountingRelay builds a relay over a fake upstream and an HTTP client that
// carries the provider's logging transport, which is where token accounting
// lives in production.
func newAccountingRelay(t *testing.T, handler http.HandlerFunc) (*admission, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: requestlog.WrapTransport(http.DefaultTransport),
	}
	relay, err := newAdmission(upstream.URL, client, nil)
	if err != nil {
		t.Fatalf("newAdmission: %v", err)
	}
	t.Cleanup(relay.close)
	return relay, upstream
}

// A real generation is recorded: the upstream hop happened, the status is the
// upstream's, and the usage from the tail of the stream is captured.
func TestAdmissionRecordsTheGeneration(t *testing.T) {
	relay, _ := newAccountingRelay(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
		fmt.Fprint(w, claudeStreamSSE)
	})

	rec := requestlog.NewRecorder("test-request", time.Now())
	relay.setRecorder(rec)

	resp, err := http.Post(relay.baseURL()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-haiku-5-5"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "message_stop") {
		t.Fatalf("answer did not reach the caller intact: %q", body)
	}

	got := rec.Snapshot()
	if !got.UpstreamCalled {
		t.Error("upstream_called is false for a turn that reached Anthropic")
	}
	if got.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", got.Status)
	}
	if got.InputTokens != 137 || got.OutputTokens != 42 {
		t.Errorf("tokens = %d/%d, want 137/42", got.InputTokens, got.OutputTokens)
	}
	if got.CacheReadTokens != 900 || got.CacheWriteTokens != 11 {
		t.Errorf("cache = %d/%d, want 900/11", got.CacheReadTokens, got.CacheWriteTokens)
	}
	if got.RateLimit["anthropic-ratelimit-unified-5h-utilization"] != "0.42" {
		t.Errorf("subscription quota headers not recorded: %v", got.RateLimit)
	}
}

// Usage is published before the handler is released: the record must already be
// complete when relay.wait returns, not a moment later.
func TestAdmissionPublishesTokensBeforeReleasingTheHandler(t *testing.T) {
	relay, _ := newAccountingRelay(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, claudeStreamSSE)
	})
	rec := requestlog.NewRecorder("test-order", time.Now())
	relay.setRecorder(rec)

	go func() {
		resp, err := http.Post(relay.baseURL()+"/v1/messages", "application/json",
			strings.NewReader(`{"model":"m"}`))
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if captured := relay.wait(5 * time.Second); captured == nil {
		t.Fatal("relay never captured the generation")
	}
	got := rec.Snapshot()
	if got.InputTokens != 137 || got.OutputTokens != 42 {
		t.Fatalf("tokens after wait = %d/%d, want 137/42: the summary would be emitted with zeros",
			got.InputTokens, got.OutputTokens)
	}
}

// Probes the CLI issues (models, health, count_tokens) are not generations and
// must leave the record alone — otherwise a probe would overwrite the real
// turn's status and token counts.
func TestAdmissionProbesDoNotTouchTheRecord(t *testing.T) {
	relay, _ := newAccountingRelay(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, claudeStreamSSE)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[]}`)
		}
	})
	rec := requestlog.NewRecorder("test-probe", time.Now())
	relay.setRecorder(rec)

	// A non-generation probe first: it must not mark the request as a call.
	resp, err := http.Get(relay.baseURL() + "/v1/models")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := rec.Snapshot()
	if got.UpstreamCalled {
		t.Error("a probe was recorded as an upstream generation")
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Errorf("probe touched token counts: %d/%d", got.InputTokens, got.OutputTokens)
	}

	// The real generation still lands afterwards, unharmed by the probe.
	resp, err = http.Post(relay.baseURL()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("generation: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got = rec.Snapshot()
	if !got.UpstreamCalled || got.InputTokens != 137 {
		t.Errorf("generation after a probe: upstream_called=%v tokens=%d, want true/137",
			got.UpstreamCalled, got.InputTokens)
	}
}

// Without a record (logging disabled) the relay must still pass traffic through.
func TestAdmissionWorksWithoutARecorder(t *testing.T) {
	relay, _ := newAccountingRelay(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, claudeStreamSSE)
	})

	resp, err := http.Post(relay.baseURL()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "message_stop") {
		t.Fatalf("relay with no recorder broke the answer: %q", body)
	}
}
