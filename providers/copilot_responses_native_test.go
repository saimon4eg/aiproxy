package providers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Claude via Copilot on /v1/responses: Responses -> Messages (native) ->
// copilot handler (fake) -> Messages -> Responses. Copilot is disabled in the
// production config, so this is the only coverage of that path.
func fakeCopilotMessages(t *testing.T, reply string, status int, gotBody *map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if gotBody != nil {
			_ = json.Unmarshal(b, gotBody)
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("copilot handler got path %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	})
}

func TestCopilotResponsesToMessages_NonStream(t *testing.T) {
	var sent map[string]any
	reply := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"hello"}],
		"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	c := &Config{copilotHandler: fakeCopilotMessages(t, reply, 200, &sent)}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"claude-x","instructions":"SYS","input":"hi"}`))
	rec := httptest.NewRecorder()
	c.makeCopilotResponsesToMessagesHandler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if sent["system"] != "SYS" {
		t.Fatalf("copilot saw system=%v", sent["system"])
	}
	if _, has := sent["cache_control"]; has {
		t.Fatal("cache_control must be stripped for Copilot")
	}
	var out struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || len(out.Output) != 1 || out.Output[0].Content[0].Text != "hello" {
		t.Fatalf("response = %s", rec.Body.String())
	}
}

func TestCopilotResponsesToMessages_Stream(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"c\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	c := &Config{copilotHandler: fakeCopilotMessages(t, sse, 200, nil)}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"c","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	c.makeCopilotResponsesToMessagesHandler().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", `"delta":"pong"`, "event: response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in stream:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestCopilotResponsesToMessages_ErrorsAndBadInput(t *testing.T) {
	c := &Config{copilotHandler: fakeCopilotMessages(t, `{"type":"error","error":{"type":"overloaded_error","message":"x"}}`, 529, nil)}
	rec := httptest.NewRecorder()
	c.makeCopilotResponsesToMessagesHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"c","input":"hi"}`)))
	if rec.Code != 529 {
		t.Fatalf("upstream error must be forwarded, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	c.makeCopilotResponsesToMessagesHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"c"}`)))
	if rec.Code != 400 {
		t.Fatalf("missing input must be 400, got %d", rec.Code)
	}
}

// A stream cut off by the upstream must still end with a terminal event.
func TestWriteAnthropicStreamAsResponses_Truncated(t *testing.T) {
	rec := httptest.NewRecorder()
	in := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"c\"}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n"
	if err := writeAnthropicStreamAsResponses(rec, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "event: response.failed") {
		t.Fatalf("truncated stream must end with response.failed:\n%s", rec.Body.String())
	}
}
