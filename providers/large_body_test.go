package providers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression: bodies above 10 MiB used to be silently truncated by
// io.LimitReader, so the upstream received broken JSON. The proxy must forward
// the body untouched and let the upstream apply its own limits.
func TestPassthroughForwardsBodyOver10MiB(t *testing.T) {
	big := `{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("a", 12<<20) + `"}]}`

	var got int
	var contentLen int
	var valid bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = len(b)
		// The proxy re-serialises JSON (key order changes), so verify that the
		// body is still valid JSON with the full content instead of comparing bytes.
		var m struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(b, &m) == nil && len(m.Messages) == 1 {
			valid = true
			contentLen = len(m.Messages[0].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	p := &ProviderConfig{
		ProviderID: "test",
		Type:       "messages",
		BaseURL:    up.URL,
		APIKey:     "k",
		AuthHeader: "bearer",
	}
	if err := p.initHTTPClient(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(big)))
	rec := httptest.NewRecorder()
	p.makePassthroughHandler("/v1/messages").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %.200s", rec.Code, rec.Body.String())
	}
	if !valid {
		t.Fatalf("upstream received invalid JSON (%d bytes) — body was truncated", got)
	}
	if contentLen != 12<<20 {
		t.Fatalf("upstream content length = %d, want %d", contentLen, 12<<20)
	}
}
