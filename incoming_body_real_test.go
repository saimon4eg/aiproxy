package main

// Check the incoming-body log shape against a body captured from live traffic.
// Run with REAL_USER_BODY=<path to a jsonl file containing a user→proxy record>.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/whtsky/copilot2api/requestlog"
)

func TestRealIncomingBodyKeepsMessagesAndModel(t *testing.T) {
	path := os.Getenv("REAL_USER_BODY")
	if path == "" {
		t.Skip("REAL_USER_BODY not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var body string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		// Any captured request body works: `user→proxy` is the client's own, and
		// `cli→relay` is what Claude Code sends — both are Anthropic-shaped requests.
		// What matters is that it parses, i.e. it is a body an elision has to keep usable.
		candidate, _ := row["body"].(string)
		if candidate == "" || !json.Valid([]byte(candidate)) {
			continue
		}
		if len(candidate) > len(body) {
			body = candidate
		}
	}
	if body == "" {
		t.Skipf("no parseable request body in %s", path)
	}

	limit := requestlog.BodyLimit()
	old := body
	if len(old) > 8<<10 {
		old = old[:8<<10]
	}
	oldKeepsModel := json.Valid([]byte(old)) && strings.Contains(old, `"model"`)
	oldKeepsMessages := json.Valid([]byte(old)) && strings.Contains(old, `"messages"`)

	elided := requestlog.ElideBody([]byte(body), limit)
	if !json.Valid([]byte(elided)) {
		t.Fatalf("the elided body is not valid JSON (%d bytes)", len(elided))
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(elided), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	messages, _ := parsed["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("`messages` did not survive: %d bytes elided from %d", len(elided), len(body))
	}
	if parsed["model"] == nil || parsed["model"] == "" {
		t.Fatalf("`model` did not survive")
	}
	if len(elided) > limit+1024 {
		t.Fatalf("elided body is %d bytes, past the %d limit", len(elided), limit)
	}
	t.Logf("live body %d bytes → head-only kept model=%v messages=%v | elided %d bytes, model=%v messages=%d",
		len(body), oldKeepsModel, oldKeepsMessages, len(elided), parsed["model"], len(messages))
}
