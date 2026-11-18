package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func planFor(t *testing.T, payload string) *claudeRequest {
	t.Helper()
	var request anthropicMessagesRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	plan, err := buildClaudeRequest(&request)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	return plan
}

// The top-level `system` field is the whole system prompt for Anthropic-shaped
// clients; it must reach the CLI.
func TestClaudeCLISystemFieldBlocksReachThePlan(t *testing.T) {
	plan := planFor(t, `{"model":"claude-sonnet-5-5","max_tokens":64,
	  "system":[{"type":"text","text":"first"},{"type":"text","text":"second"}],
	  "messages":[{"role":"user","content":"hi"}]}`)
	if plan.System != "first\n\nsecond" {
		t.Fatalf("system = %q", plan.System)
	}
}

func TestClaudeCLISystemFieldAsPlainString(t *testing.T) {
	plan := planFor(t, `{"model":"claude-sonnet-5-5","max_tokens":64,
	  "system":"plain prompt","messages":[{"role":"user","content":"hi"}]}`)
	if plan.System != "plain prompt" {
		t.Fatalf("system = %q", plan.System)
	}
}

// Upstream orders the top-level field before any system-role message.
func TestClaudeCLISystemFieldPrecedesSystemMessages(t *testing.T) {
	plan := planFor(t, `{"model":"claude-sonnet-5-5","max_tokens":64,
	  "system":[{"type":"text","text":"top"}],
	  "messages":[{"role":"system","content":"from message"},{"role":"user","content":"hi"}]}`)
	if plan.System != "top\n\nfrom message" {
		t.Fatalf("system = %q", plan.System)
	}
}

// Requests that never used the field keep exactly the old behaviour.
func TestClaudeCLISystemMessagesOnlyUnchanged(t *testing.T) {
	plan := planFor(t, `{"model":"claude-sonnet-5-5","max_tokens":64,
	  "messages":[{"role":"system","content":"only message"},{"role":"user","content":"hi"}]}`)
	if plan.System != "only message" {
		t.Fatalf("system = %q", plan.System)
	}
	plan = planFor(t, `{"model":"claude-sonnet-5-5","max_tokens":64,
	  "messages":[{"role":"user","content":"hi"}]}`)
	if plan.System != "" {
		t.Fatalf("no system anywhere must stay empty, got %q", plan.System)
	}
}

// Blocks that are not plain text (cache directives ride on text blocks, other
// types carry no prompt text) never leak into the prompt, and empty text is skipped.
func TestClaudeCLISystemFieldIgnoresEmptyAndNonText(t *testing.T) {
	plan := planFor(t, `{"model":"claude-sonnet-5-5","max_tokens":64,
	  "system":[{"type":"text","text":"keep","cache_control":{"type":"ephemeral"}},
	            {"type":"text","text":""},{"type":"image","text":"skip"}],
	  "messages":[{"role":"user","content":"hi"}]}`)
	if plan.System != "keep" {
		t.Fatalf("system = %q", plan.System)
	}
}

// Same input, same bytes: the prompt is the head of the cached prefix.
func TestClaudeCLISystemFieldIsDeterministic(t *testing.T) {
	const payload = `{"model":"claude-sonnet-5-5","max_tokens":64,
	  "system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],
	  "messages":[{"role":"system","content":"c"},{"role":"user","content":"hi"}]}`
	first := planFor(t, payload).System
	for i := 0; i < 5; i++ {
		if got := planFor(t, payload).System; got != first {
			t.Fatalf("run %d produced %q, want %q", i, got, first)
		}
	}
}

func TestClaudeCLISystemFieldTooLargeIsRefused(t *testing.T) {
	huge := strings.Repeat("x", maxSystemLength+1)
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-5-5", "max_tokens": 64,
		"system":   huge,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	var request anthropicMessagesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if _, err := buildClaudeRequest(&request); err == nil {
		t.Fatal("an oversized system prompt must be refused")
	}
}

// End to end: the CLI is launched with --system-prompt-file and the file holds
// the client's system prompt.
func TestClaudeCLISystemFieldReachesSystemPromptFile(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, fake := testProvider(t, server.URL, map[string]any{"mode": "text"})
	writeFakeConfig(t, fake, map[string]any{
		"mode": "text", "dump_argv": fake.path("argv.json"), "dump_system": fake.path("system.txt"),
	})
	const marker = "SECRET-WORD-ZEBRA-7731"
	body := `{"model":"claude-sonnet-5-5","max_tokens":64,"stream":true,
	  "system":[{"type":"text","text":"` + marker + `"}],
	  "messages":[{"role":"user","content":"hi"}]}`
	if rec := post(t, p, body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(fake.path("system.txt"))
	if err != nil {
		t.Fatalf("system prompt file was not captured: %v", err)
	}
	if !strings.Contains(string(data), marker) {
		t.Fatalf("system prompt file lacks the client's system field: %q", data)
	}
}
