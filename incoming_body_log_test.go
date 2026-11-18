package main

// The incoming body is the only copy of what the client actually asked for, and it is
// shared by every provider — so its log line has to stay useful for a large request.
// A head-only cut keeps the model and the start of `system` and drops `messages`, which
// is the part every investigation needs; the body is elided structurally instead, so
// both ends survive and the line stays parseable JSON.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whtsky/copilot2api/requestlog"
)

const incomingMarker = "TAIL-MARKER-9f21"

// bigIncomingBody is a request shaped like the ones clients send: a model, a system
// block and a long `messages` array whose LAST entry carries a unique marker.
func bigIncomingBody(t *testing.T) string {
	t.Helper()
	messages := make([]map[string]any, 0, 60)
	for i := 0; i < 59; i++ {
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": fmt.Sprintf("turn %d: %s", i, strings.Repeat("padding text ", 600)),
		})
	}
	messages = append(messages, map[string]any{
		"role":    "user",
		"content": "the newest turn, which must survive the cut: " + incomingMarker,
	})
	body, err := json.Marshal(map[string]any{
		"model":    "claude-sonnet-5-5",
		"stream":   true,
		"system":   []any{map[string]any{"type": "text", "text": strings.Repeat("system rule ", 200)}},
		"messages": messages,
	})
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	if len(body) < 4*requestlog.BodyLimit() {
		t.Fatalf("fixture is only %d bytes; it has to be far larger than the limit (%d)",
			len(body), requestlog.BodyLimit())
	}
	return string(body)
}

// The user→proxy line keeps BOTH ends of a large incoming body and remains valid JSON.
func TestIncomingBodyLogKeepsBothEnds(t *testing.T) {
	dir := t.TempDir()
	requestlog.Init(requestlog.Config{Dir: dir, Bodies: true, Index: false})

	body := bigIncomingBody(t)
	handler := requestIDMiddleware(debugLogMiddleware(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	logged := incomingBodyFromLog(t, dir)
	if len(logged) > requestlog.BodyLimit()+1024 {
		t.Fatalf("logged body is %d bytes, far past the %d limit", len(logged), requestlog.BodyLimit())
	}
	if !strings.Contains(logged, "claude-sonnet-5-5") {
		t.Fatalf("the head of the body was lost (no model):\n%s", head(logged))
	}
	if !strings.Contains(logged, incomingMarker) {
		t.Fatalf("the tail of the body was lost: the newest turn is not in the log, which is the "+
			"head-only cut this replaces. Logged %d bytes:\n%s", len(logged), head(logged))
	}
	if !json.Valid([]byte(logged)) {
		t.Fatalf("logged body is not valid JSON, so nothing can parse it:\n%s", head(logged))
	}

	// The line has to stay parseable, so the elision is structural, not a byte cut.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(logged), &parsed); err != nil {
		t.Fatalf("logged body does not decode: %v", err)
	}
	if parsed["model"] != "claude-sonnet-5-5" {
		t.Fatalf("model lost from the decoded body: %v", parsed["model"])
	}
	if messages, ok := parsed["messages"].([]any); !ok || len(messages) == 0 {
		t.Fatalf("`messages` did not survive: %T", parsed["messages"])
	}
}

// A body that fits is logged verbatim — elision must not touch a normal small request.
func TestSmallIncomingBodyIsLoggedVerbatim(t *testing.T) {
	dir := t.TempDir()
	requestlog.Init(requestlog.Config{Dir: dir, Bodies: true, Index: false})

	body := `{"model":"claude-haiku-5-5","max_tokens":8,"messages":[{"role":"user","content":"Say: OK"}]}`
	handler := requestIDMiddleware(debugLogMiddleware(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := incomingBodyFromLog(t, dir); got != body {
		t.Fatalf("a body within the limit was altered\n  got : %s\n  want: %s", got, body)
	}
}

// incomingBodyFromLog returns the `body` field of the user→proxy record.
func incomingBodyFromLog(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no log files under %s (err %v)", dir, err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var row map[string]any
			if json.Unmarshal([]byte(line), &row) != nil {
				continue
			}
			if row["dir"] != "user→proxy" {
				continue
			}
			body, _ := row["body"].(string)
			return body
		}
	}
	t.Fatal("no user→proxy record in the log")
	return ""
}

func head(s string) string {
	if len(s) <= 400 {
		return s
	}
	return s[:400] + "…"
}
