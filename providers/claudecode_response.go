package providers

// Native stream-json → Anthropic Messages responses.
//
// Two paths share one accumulator: a real SSE client gets deltas as they arrive,
// and a non-streaming client gets the same content assembled at the end. The
// authoritative stop reason and usage come from the native `result` event, not
// from the last delta seen.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// answer collects everything one native turn produced.
type answer struct {
	assistantMessages []map[string]any
	results           []map[string]any
	emittedText       strings.Builder
	sawMessageStop    bool
	nativeError       string
	nativeErrorCode   string
}

// sseWriter emits Anthropic server-sent events.
//
// The 200 is committed lazily, on the first event, not at construction: the
// upstream status is the host's retry signal, so a refusal that arrives before
// any content must be answerable with its own status instead of a committed 200
// whose body merely mentions a failure.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	wrote   bool
	// preamble is written once, right after the headers commit and before the
	// first real event — the events every Anthropic client expects to see first.
	preamble []map[string]any
	// before runs once, immediately before the headers commit. Returning an error
	// aborts the stream so the caller can still answer with a real HTTP status.
	before func() error
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	flusher, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: flusher}
}

// send writes one named event. The event name is the payload's own "type", which
// is what every Anthropic client expects.
func (s *sseWriter) send(payload map[string]any) error {
	if !s.wrote {
		if s.before != nil {
			hook := s.before
			s.before = nil
			if err := hook(); err != nil {
				return err
			}
		}
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("Connection", "keep-alive")
		s.w.WriteHeader(http.StatusOK)
		s.wrote = true
		for _, event := range s.preamble {
			if err := s.write(event); err != nil {
				return err
			}
		}
	}
	return s.write(payload)
}

// write emits one event without touching the header state.
func (s *sseWriter) write(payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	kind, _ := payload["type"].(string)
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", kind, data); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

// textDelta streams one text fragment.
func (a *answer) textDelta(index int, text string) map[string]any {
	a.emittedText.WriteString(text)
	return map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	}
}

// thinkingDelta streams one reasoning fragment.
func thinkingDelta(index int, thinking string) map[string]any {
	return map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
	}
}

// blocks returns the assistant content blocks across every native message.
func (a *answer) blocks() []map[string]any {
	var out []map[string]any
	for _, message := range a.assistantMessages {
		content, _ := message["content"].([]any)
		for _, raw := range content {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, block)
		}
	}
	return out
}

// finalText joins every text block.
func (a *answer) finalText() string {
	var builder strings.Builder
	for _, block := range a.blocks() {
		if block["type"] == "text" {
			if text, ok := block["text"].(string); ok {
				builder.WriteString(text)
			}
		}
	}
	return builder.String()
}

// toolCalls returns native tool_use blocks with host tool names restored.
func (a *answer) toolCalls(plan *claudeRequest) []map[string]any {
	var calls []map[string]any
	for _, block := range a.blocks() {
		if block["type"] != "tool_use" {
			continue
		}
		name, _ := block["name"].(string)
		input, _ := block["input"].(map[string]any)
		if input == nil {
			input = map[string]any{}
		}
		calls = append(calls, map[string]any{
			"type":  "tool_use",
			"id":    block["id"],
			"name":  plan.hostToolName(name),
			"input": input,
		})
	}
	return calls
}

// usageFrom merges native token accounting into Anthropic's field names.
func (a *answer) usageFrom(native map[string]any) map[string]any {
	extract := func(source map[string]any) (int, int, int, int) {
		number := func(key string) int {
			if value, ok := source[key].(float64); ok {
				return int(value)
			}
			return 0
		}
		return number("input_tokens"), number("output_tokens"),
			number("cache_read_input_tokens"), number("cache_creation_input_tokens")
	}

	input, output, cacheRead, cacheWrite := 0, 0, 0, 0
	if usage, ok := native["usage"].(map[string]any); ok && len(usage) > 0 {
		input, output, cacheRead, cacheWrite = extract(usage)
	} else if len(a.results) > 0 {
		if usage, ok := a.results[0]["usage"].(map[string]any); ok {
			input, output, cacheRead, cacheWrite = extract(usage)
		}
	}

	return map[string]any{
		"input_tokens":                input,
		"output_tokens":               output,
		"cache_read_input_tokens":     cacheRead,
		"cache_creation_input_tokens": cacheWrite,
	}
}

// stopReason maps native's stop reason onto Anthropic's vocabulary.
func (a *answer) stopReason() string {
	hasTools := false
	for _, block := range a.blocks() {
		if block["type"] == "tool_use" {
			hasTools = true
			break
		}
	}
	reason := ""
	for _, message := range a.assistantMessages {
		if value, ok := message["stop_reason"].(string); ok && value != "" {
			reason = value
			break
		}
	}
	switch reason {
	case "max_tokens", "model_context_window_exceeded":
		return "max_tokens"
	case "refusal":
		return "refusal"
	case "tool_use":
		return "tool_use"
	}
	if hasTools {
		return "tool_use"
	}
	return "end_turn"
}

// refusalReason pulls the explanation out of a native refusal.
func (a *answer) refusalReason() string {
	for _, message := range a.assistantMessages {
		details, ok := message["stop_details"].(map[string]any)
		if !ok {
			continue
		}
		if explanation, ok := details["explanation"].(string); ok && strings.TrimSpace(explanation) != "" {
			return strings.TrimSpace(explanation)
		}
		if category, ok := details["category"].(string); ok && category != "" {
			return "provider refusal category: " + category
		}
	}
	return ""
}

// buildResponse assembles the non-streaming body.
func (a *answer) buildResponse(model string, plan *claudeRequest) map[string]any {
	stop := a.stopReason()
	blocks := []map[string]any{}

	switch stop {
	case "tool_use":
		// Order matters: reasoning precedes the text it reasoned about, and both
		// precede the call. Signed thinking must be kept so history replays.
		for _, block := range a.blocks() {
			if block["type"] == "thinking" && block["signature"] != nil {
				blocks = append(blocks, block)
			}
		}
		if text := a.finalText(); strings.TrimSpace(text) != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
		blocks = append(blocks, a.toolCalls(plan)...)
	case "refusal":
		blocks = append(blocks, map[string]any{"type": "text", "text": a.refusalReason()})
	default:
		for _, block := range a.blocks() {
			if block["type"] == "thinking" && block["signature"] != nil {
				blocks = append(blocks, block)
			}
		}
		if text := a.finalText(); strings.TrimSpace(text) != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}

	response := map[string]any{
		"id":          nativeMessageID(a),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     blocks,
		"stop_reason": stop,
		"usage":       a.usageFrom(map[string]any{}),
	}
	if stop == "refusal" {
		response["stop_details"] = map[string]any{"explanation": a.refusalReason()}
	}
	if cost := nativeCost(a); cost != nil {
		// The CLI's list-price accounting is useful diagnostics, but it is not part
		// of the Anthropic Messages shape. Publishing it inline would make clients
		// that validate the schema reject the whole message, so it goes to the log.
		slog.Debug("claude-cli turn cost", "provider_turn_cost_usd", cost["total_cost_usd"])
	}
	return response
}

func nativeMessageID(a *answer) string {
	for _, message := range a.assistantMessages {
		if id, ok := message["id"].(string); ok && id != "" {
			return id
		}
	}
	return "msg_claude_cli"
}

// nativeCost preserves the CLI's list-price accounting as metadata. It is NOT a
// subscription charge; publishing it as one would be a false claim.
func nativeCost(a *answer) map[string]any {
	if len(a.results) == 0 {
		return nil
	}
	result := a.results[0]
	var total any
	if value, ok := result["total_cost_usd"]; ok {
		total = value
	}
	models, _ := result["modelUsage"].(map[string]any)
	if total == nil && models == nil {
		return nil
	}
	return map[string]any{"total_cost_usd": total, "model_usage": models}
}
