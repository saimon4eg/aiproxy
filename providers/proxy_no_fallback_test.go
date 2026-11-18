package providers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A configured proxy is a hard requirement: if it is unreachable the request must
// fail, never silently connect around it. This is the regression guard for the
// `u, _ := ParseProxyURL(...)` shape, where a parse error degraded into
// http.ProxyURL(nil) — which Go documents as a direct connection.
func TestConfiguredProxyIsNeverBypassed(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	p := ProviderConfig{
		ProviderID:  "proxy-required",
		Type:        "messages",
		BaseURL:     upstream.URL,
		APIKey:      "provider-key",
		APIPrefix:   "/v1",
		ModelPrefix: "proxy-required",
		// Port 1 refuses immediately; nothing listens there.
		ProxyHost: "127.0.0.1:1",
	}
	if err := p.initHTTPClient(); err != nil {
		t.Fatalf("initHTTPClient: %v", err)
	}

	handler := p.makePassthroughHandler("/v1/messages")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"proxy-required-x","max_tokens":1,"messages":[]}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got := atomic.LoadInt32(&upstreamHits); got != 0 {
		t.Fatalf("upstream was reached %d time(s) despite a dead proxy — direct fallback happened", got)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("request succeeded (status %d) through a dead proxy", rec.Code)
	}
}

// The same guarantee on the model-listing path, which builds its own client.
func TestConfiguredProxyIsNeverBypassedForModels(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer upstream.Close()

	p := ProviderConfig{
		ProviderID:  "proxy-required-models",
		Type:        "messages",
		BaseURL:     upstream.URL,
		APIKey:      "provider-key",
		ModelPrefix: "proxy-required-models",
		ProxyHost:   "127.0.0.1:1",
	}

	if _, err := FetchModels(t.Context(), p); err == nil {
		t.Fatal("FetchModels succeeded through a dead proxy")
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 0 {
		t.Fatalf("upstream was reached %d time(s) despite a dead proxy — direct fallback happened", got)
	}
}

// An unparsable proxy_host must be a hard error, not a silent degrade.
func TestUnparsableProxyHostFailsInit(t *testing.T) {
	p := ProviderConfig{
		ProviderID:  "bad-proxy",
		Type:        "messages",
		BaseURL:     "https://example.invalid",
		APIKey:      "k",
		ModelPrefix: "bad-proxy",
		ProxyHost:   "http://[::1]:namedport",
	}
	if err := p.initHTTPClient(); err == nil {
		t.Fatal("initHTTPClient accepted an unparsable proxy_host")
	}
	if p.client != nil {
		t.Fatal("client was built despite an invalid proxy_host")
	}
}
