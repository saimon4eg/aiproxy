package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// marksOf lists every cache_control in a decoded request body, keyed by where it
// sits: "tools", "system" or "messages[<i>].content[<j>]".
func marksOf(t *testing.T, body map[string]any) []string {
	t.Helper()
	var marks []string
	for _, section := range []string{"tools", "system"} {
		items, _ := body[section].([]any)
		for i, raw := range items {
			if block, ok := raw.(map[string]any); ok {
				if _, has := block["cache_control"]; has {
					marks = append(marks, fmt.Sprintf("%s[%d]", section, i))
				}
			}
		}
	}
	messages, _ := body["messages"].([]any)
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, _ := message["content"].([]any)
		for j, item := range content {
			if block, ok := item.(map[string]any); ok {
				if _, has := block["cache_control"]; has {
					marks = append(marks, fmt.Sprintf("messages[%d].content[%d]", i, j))
				}
			}
		}
	}
	return marks
}

func messageMarks(t *testing.T, body map[string]any) []string {
	t.Helper()
	var marks []string
	for _, mark := range marksOf(t, body) {
		if strings.HasPrefix(mark, "messages[") {
			marks = append(marks, mark)
		}
	}
	return marks
}

func decodeBody(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	body, err := decodeWireBody(payload)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

// decodeQueried turns literal JSON into the decoded shape the helpers compare
// against.
func decodeQueried(t *testing.T, payload string) []any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	var blocks []any
	if err := decoder.Decode(&blocks); err != nil {
		t.Fatalf("decode queried: %v", err)
	}
	return blocks
}

// The 2.1.287+ shape after the frame was restored: the CLI marked the last
// tool_use of the turn AND its trailing per-request `role: system` message. Only
// the restored tool_result recurs, so the breakpoint belongs there and the
// second mark must be folded away — otherwise every round is written twice.
const restoredWireBody = `{
  "model": "claude-sonnet-5-5",
  "tools": [
    {"name": "mcp__hermes__a", "input_schema": {"type": "object"}},
    {"name": "mcp__hermes__b", "input_schema": {"type": "object"}, "cache_control": {"type": "ephemeral"}}
  ],
  "system": [{"type": "text", "text": "system prompt", "cache_control": {"type": "ephemeral"}}],
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "q0"}]},
    {"role": "assistant", "content": [
      {"type": "text", "text": "a0"},
      {"type": "tool_use", "id": "t1", "name": "mcp__hermes__a", "input": {}, "cache_control": {"type": "ephemeral"}}
    ]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "res"}]},
    {"role": "system", "content": [{"type": "text", "text": "per-request reminder", "cache_control": {"type": "ephemeral"}}]}
  ]
}`

var restoredQueried = `[{"type": "tool_result", "tool_use_id": "t1", "content": "res"}]`

func TestPinMessageBreakpointFoldsDuplicateMarksOntoTheRecurringBlock(t *testing.T) {
	queried := decodeQueried(t, restoredQueried)
	patched := pinMessageBreakpoint([]byte(restoredWireBody), queried)
	body := decodeBody(t, patched)

	// Marks on content the next request replays stay; the mark on the trailing
	// per-request `role: system` message — which never recurs — is folded onto
	// the restored tool_result instead of being written again every round.
	want := []string{"messages[1].content[1]", "messages[2].content[0]"}
	if got := messageMarks(t, body); !reflect.DeepEqual(got, want) {
		t.Fatalf("message-level marks = %v, want %v", got, want)
	}
	all := marksOf(t, body)
	if len(all) > 4 {
		t.Fatalf("cache breakpoints = %d (%v), Anthropic allows four", len(all), all)
	}
	// Tools and system marks are never touched.
	if all[0] != "tools[1]" || all[1] != "system[0]" {
		t.Fatalf("tools/system marks changed: %v", all)
	}
	// The never-recurring reminder no longer carries a breakpoint.
	trailing, _ := body["messages"].([]any)[3].(map[string]any)
	if block, _ := trailing["content"].([]any)[0].(map[string]any); block != nil {
		if _, has := block["cache_control"]; has {
			t.Fatalf("trailing per-request message still marked: %v", block)
		}
	}
	// The moved marker keeps native's own directive.
	message, _ := body["messages"].([]any)[2].(map[string]any)
	block, _ := message["content"].([]any)[0].(map[string]any)
	directive, _ := block["cache_control"].(map[string]any)
	if kind, _ := directive["type"].(string); kind != "ephemeral" {
		t.Fatalf("moved marker lost its directive: %v", block["cache_control"])
	}
}

// Without restoration the tool_result no longer matches, so the span ends at the
// assistant turn: the breakpoint stays on the tool_use, which the next request
// does replay, and the trailing system mark is still folded away.
func TestPinMessageBreakpointFallsBackToTheAssistantTurn(t *testing.T) {
	unrestorable := strings.Replace(restoredWireBody,
		`{"type": "tool_result", "tool_use_id": "t1", "content": "res"}`,
		`{"type": "tool_result", "tool_use_id": "other", "content": "res"}`, 1)
	queried := decodeQueried(t, restoredQueried)

	patched := pinMessageBreakpoint([]byte(unrestorable), queried)
	body := decodeBody(t, patched)

	marks := messageMarks(t, body)
	if len(marks) != 1 {
		t.Fatalf("message-level marks = %v, want exactly one", marks)
	}
	if marks[0] != "messages[1].content[1]" {
		t.Fatalf("breakpoint landed on %s, want the assistant tool_use messages[1].content[1]", marks[0])
	}
}

// A body that is not a Messages payload forwards byte for byte.
func TestPinMessageBreakpointLeavesUnparsablePayloadAlone(t *testing.T) {
	queried := decodeQueried(t, restoredQueried)
	for _, payload := range []string{`not json at all`, `{"messages": "nope"}`, `{"model":"m"}`} {
		if got := pinMessageBreakpoint([]byte(payload), queried); string(got) != payload {
			t.Fatalf("payload %q was rewritten to %q", payload, got)
		}
	}
}

// The CLI echoes the queried frame with its own reminder appended to the last
// tool result; restoring the host's representation is what makes the next
// request's prefix match again.
func TestRestoreQueriedTurnDropsNativeAddition(t *testing.T) {
	native := strings.Replace(restoredWireBody,
		`{"type": "tool_result", "tool_use_id": "t1", "content": "res"}`,
		`{"type": "tool_result", "tool_use_id": "t1", "content": "res\n\n<reminder>today is Tuesday</reminder>"}`, 1)
	queried := decodeQueried(t, restoredQueried)

	restored, err := restoreQueriedTurn([]byte(native), queried)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	body := decodeBody(t, restored)
	message, _ := body["messages"].([]any)[2].(map[string]any)
	block, _ := message["content"].([]any)[0].(map[string]any)
	if content, _ := block["content"].(string); content != "res" {
		t.Fatalf("tool_result content = %q, want the host's %q", content, "res")
	}
}

// A frame the helper cannot prove lossless is a hard mismatch, not a guess: the
// caller forwards the CLI's body untouched.
func TestRestoreQueriedTurnRefusesForeignFrames(t *testing.T) {
	queried := decodeQueried(t, restoredQueried)
	foreign := strings.Replace(restoredWireBody,
		`{"type": "tool_result", "tool_use_id": "t1", "content": "res"}`,
		`{"type": "tool_result", "tool_use_id": "t2", "content": "res"}`, 1)
	if _, err := restoreQueriedTurn([]byte(foreign), queried); err == nil {
		t.Fatal("restoring a frame anchored on different tool_use_id should fail")
	}

	// A user-only frame is outside restoration scope and stays byte-identical.
	plain := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"a"}]},{"role":"user","content":[{"type":"text","text":"q"}]}]}`)
	userOnly := decodeQueried(t, `[{"type":"text","text":"q"}]`)
	if got, err := restoreQueriedTurn(plain, userOnly); err != nil || string(got) != string(plain) {
		t.Fatalf("user-only frame changed: %q, %v", got, err)
	}
}

// queriedFrame must normalise numbers the way a decoded body carries them, or
// equality between the two sides silently never holds.
func TestQueriedFrameNormalisesNumbers(t *testing.T) {
	frames := []nativeFrame{
		{Type: "assistant", Message: map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "a"}}}},
		{Type: "user", Message: map[string]any{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "t1", "content": []map[string]any{
				{"type": "text", "text": "rows", "count": 3},
			}},
		}}},
	}
	blocks := queriedFrame(frames)
	if len(blocks) != 1 {
		t.Fatalf("queried blocks = %d, want 1", len(blocks))
	}
	decoded := decodeBody(t, []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"rows","count":3}]}]}]}`))
	messages, _ := decoded["messages"].([]any)
	content, _ := messages[0].(map[string]any)["content"].([]any)
	if !reflect.DeepEqual(blocks, content) {
		t.Fatalf("queried %#v does not equal the decoded body %#v", blocks, content)
	}

	// A trailing assistant frame has no querying turn to pin.
	if got := queriedFrame(frames[:1]); got != nil {
		t.Fatalf("queriedFrame of an assistant-only history = %#v, want nil", got)
	}
}

// Hermes asks for `thinking: {type: adaptive, display: summarized}`; the CLI
// receives the config through CLAUDE_CODE_EXTRA_BODY. Dropping `display` makes
// 4.7+ answer with empty, signature-only thinking blocks, so the client shows no
// reasoning at all.
func TestBuildClaudeRequestCarriesThinkingDisplay(t *testing.T) {
	payload := `{"model":"claude-sonnet-5-5","max_tokens":1024,
	  "thinking":{"type":"adaptive","display":"summarized"},
	  "output_config":{"effort":"high"},
	  "messages":[{"role":"user","content":[{"type":"text","text":"think about it"}]}]}`
	var request anthropicMessagesRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	if request.Thinking == nil || request.Thinking.Display != "summarized" {
		t.Fatalf("thinking.display was not decoded: %#v", request.Thinking)
	}
	plan, err := buildClaudeRequest(&request)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	extra := decodeBody(t, []byte(plan.ExtraBody))
	thinking, _ := extra["thinking"].(map[string]any)
	if kind, _ := thinking["type"].(string); kind != "adaptive" {
		t.Fatalf("thinking.type = %v, want adaptive", thinking["type"])
	}
	if display, _ := thinking["display"].(string); display != "summarized" {
		t.Fatalf("thinking.display = %v, want summarized (dropping it hides all reasoning)", thinking["display"])
	}
	if effort := plan.Effort; effort != "high" {
		t.Fatalf("effort = %q, want high", effort)
	}
}

// A client that asks for thinking with an explicit budget keeps it, and one that
// disables thinking sends no thinking block at all.
func TestBuildClaudeRequestThinkingBudgetAndDisable(t *testing.T) {
	budgeted := `{"model":"claude-opus-5-5","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":4096},
	  "messages":[{"role":"user","content":[{"type":"text","text":"q"}]}]}`
	var request anthropicMessagesRequest
	if err := json.Unmarshal([]byte(budgeted), &request); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	plan, err := buildClaudeRequest(&request)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	thinking, _ := decodeBody(t, []byte(plan.ExtraBody))["thinking"].(map[string]any)
	if budget, _ := thinking["budget_tokens"].(json.Number); budget.String() != "4096" {
		t.Fatalf("budget_tokens = %v, want 4096", thinking["budget_tokens"])
	}

	disabled := `{"model":"claude-opus-5-5","max_tokens":64,"thinking":{"type":"disabled"},
	  "messages":[{"role":"user","content":[{"type":"text","text":"q"}]}]}`
	request = anthropicMessagesRequest{}
	if err := json.Unmarshal([]byte(disabled), &request); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	plan, err = buildClaudeRequest(&request)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if _, has := decodeBody(t, []byte(plan.ExtraBody))["thinking"]; has {
		t.Fatal("a disabled thinking config must not be forwarded")
	}
}

// The corrected body must still be a payload Anthropic accepts: it starts with a
// user turn, carries no role the Messages API does not know, has no empty
// content, keeps the client's system prompt and tool inventory untouched, and
// stays within the four-breakpoint limit. This is the "nothing breaks" check the
// scrubbing needs.
func TestCorrectedBodyIsAValidMessagesPayload(t *testing.T) {
	// Neutral synthetic content: what is asserted is this package's arithmetic —
	// which blocks survive, where the breakpoint lands — not anything a specific
	// client version happens to emit.
	const preamble = "prepended context block\n"
	const reminder = "appended annotation\n"
	native := `{"model":"claude-sonnet-5-5","max_tokens":512,
	  "system":[{"type":"text","text":"system block 0"},{"type":"text","text":"client system","cache_control":{"type":"ephemeral"}}],
	  "tools":[{"name":"mcp__hermes__a","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":` + mustJSON(preamble) + `},{"type":"text","text":"read the config"}]},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"plan","signature":"sig1"},
	      {"type":"tool_use","id":"toolu_1","name":"mcp__hermes__a","input":{},"cache_control":{"type":"ephemeral"}}
	    ]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"port: 8081"},{"type":"text","text":` + mustJSON(reminder) + `}]}]},
	    {"role":"system","content":[{"type":"text","text":"trailing context message","cache_control":{"type":"ephemeral"}}]}
	  ]}`
	queried := decodeQueried(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"port: 8081"}]}]`)

	relay := &admission{}
	relay.setQueried(queried)
	corrected := relay.applyCacheWire([]byte(native))

	body := decodeBody(t, corrected)

	// NOTHING the CLI built is removed. The prepended reminder block, every
	// message and the trailing `role: system` turn all survive — the same rule
	// the plugin's own tests pin (`[FRAME, REMINDER]` reordered, not dropped; the
	// trailing system message forwarded unchanged).
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tool inventory changed: %#v", tools)
	}
	system, _ := body["system"].([]any)
	if len(system) != 2 {
		t.Fatalf("system prompt changed: %d blocks", len(system))
	}
	if text, _ := system[1].(map[string]any)["text"].(string); text != "client system" {
		t.Fatalf("system block text changed: %q", text)
	}

	messages, _ := body["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want all four the CLI sent", len(messages))
	}
	if role, _ := messages[3].(map[string]any)["role"].(string); role != "system" {
		t.Fatalf("the CLI's trailing context message was removed: %#v", messages[3])
	}
	for i, raw := range messages {
		content, ok := raw.(map[string]any)["content"].([]any)
		if !ok || len(content) == 0 {
			t.Fatalf("message %d has empty content", i)
		}
	}

	// The single rewrite: the queried tool result goes back to the client's own
	// representation, which is exactly what restoreQueriedTurn is for.
	resultMessage, _ := messages[2].(map[string]any)
	resultContent, _ := resultMessage["content"].([]any)
	resultBlock, _ := resultContent[0].(map[string]any)
	wanted := queried[0].(map[string]any)["content"]
	if !reflect.DeepEqual(resultBlock["content"], wanted) {
		t.Fatalf("tool_result content = %#v, want the client's %#v", resultBlock["content"], wanted)
	}

	// Breakpoint budget: four across tools, system and messages.
	if total := len(marksOf(t, body)); total > 4 {
		t.Fatalf("%d breakpoints, Anthropic allows four: %v", total, marksOf(t, body))
	}
	// And the cached span still ends on the replayed tool result.
	if marks := messageMarks(t, body); len(marks) == 0 ||
		marks[len(marks)-1] != "messages[2].content[0]" {
		t.Fatalf("message marks = %v, want the last one on the replayed tool result", marks)
	}
}

// A relay with no queried frame never rewrites anything — the plugin behaved the
// same way (`if not queried: return payload`).
func TestPinAndRestoreAreNoOpsWithoutAQuery(t *testing.T) {
	const native = `{"messages":[
	  {"role":"user","content":[{"type":"text","text":"prepended context block"},{"type":"text","text":"q"}]},
	  {"role":"system","content":[{"type":"text","text":"trailing context message","cache_control":{"type":"ephemeral"}}]}
	]}`
	if got := pinMessageBreakpoint([]byte(native), nil); string(got) != native {
		t.Fatalf("pin rewrote a body with no queried frame: %s", got)
	}
	restored, err := restoreQueriedTurn([]byte(native), nil)
	if err != nil || string(restored) != native {
		t.Fatalf("restore touched a body with no queried frame: %s (%v)", restored, err)
	}
}

// The relay's correction is only as good as the frame handed to it: the handler
// derives it from the plan it replays, so that derivation is pinned here against
// a real client payload rather than a hand-built slice.
func TestQueriedFrameComesFromTheReplayedPlan(t *testing.T) {
	payload := `{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[
	  {"role":"user","content":[{"type":"text","text":"read the config"}]},
	  {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"config.yaml"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"port: 8081"}]}]}]}`
	var request anthropicMessagesRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	plan, err := buildClaudeRequest(&request)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	blocks := queriedFrame(plan.Frames)
	want := decodeQueried(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"port: 8081"}]}]`)
	if !reflect.DeepEqual(blocks, want) {
		t.Fatalf("queried frame = %#v, want %#v", blocks, want)
	}

	// And the pair really works on a body the CLI would send back: the reminder
	// the CLI appends to that result is removed, so the next request's prefix
	// matches again.
	native := `{"messages":[
	  {"role":"user","content":[{"type":"text","text":"read the config"}]},
	  {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"mcp__hermes__read_file","input":{"path":"config.yaml"},"cache_control":{"type":"ephemeral"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"port: 8081"},{"type":"text","text":"\n\n<reminder>today</reminder>"}]}]},
	  {"role":"system","content":[{"type":"text","text":"per-request context","cache_control":{"type":"ephemeral"}}]}]}`
	restored, err := restoreQueriedTurn([]byte(native), blocks)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	body := decodeBody(t, restored)
	message, _ := body["messages"].([]any)[2].(map[string]any)
	content, _ := message["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("restored content = %#v, want the host's single block", content)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["content"].([]any)[0].(map[string]any)["text"].(string)
	if text != "port: 8081" {
		t.Fatalf("tool_result text = %q, want the host's text", text)
	}

	pinned := pinMessageBreakpoint(restored, blocks)
	if marks := messageMarks(t, decodeBody(t, pinned)); !reflect.DeepEqual(marks,
		[]string{"messages[1].content[0]", "messages[2].content[0]"}) {
		t.Fatalf("message marks after pin = %v, want the breakpoint on the restored result", marks)
	}
}

// The whole point of the port: the relay itself must apply the correction. A
// test that stays green with the applyCacheWire call removed proves nothing.
func TestAdmissionAppliesCacheWireToTheGeneration(t *testing.T) {
	var received []byte
	var probeBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/messages":
			received = body
		case "/api/hello":
			probeBody = body
		default:
			t.Errorf("upstream got path %q", r.URL.Path)
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	relay, err := newAdmission(upstream.URL, &http.Client{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()
	relay.setQueried(decodeQueried(t, restoredQueried))

	response, err := http.Post(relay.baseURL()+"/v1/messages", "application/json",
		strings.NewReader(restoredWireBody))
	if err != nil {
		t.Fatalf("generation: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("generation status = %d", response.StatusCode)
	}
	if len(received) == 0 {
		t.Fatal("upstream never saw the generation")
	}
	if string(received) == restoredWireBody {
		t.Fatal("relay forwarded the CLI body untouched: cache wire is not wired in")
	}
	body := decodeBody(t, received)
	// Native's per-request context is gone, and the two marks left are the ones
	// on content the next request replays: the assistant tool_use and, because
	// restoration ran first, the restored tool result at the end of the prefix.
	if marks := messageMarks(t, body); !reflect.DeepEqual(marks, []string{"messages[1].content[1]", "messages[2].content[0]"}) {
		t.Fatalf("upstream saw message marks %v, want the two recurring ones", marks)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want all four the CLI sent", len(messages))
	}

	probe, err := http.Post(relay.baseURL()+"/api/hello", "application/json",
		strings.NewReader(`{"probe":true}`))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	probe.Body.Close()
	if string(probeBody) != `{"probe":true}` {
		t.Fatalf("probe body was rewritten: %q", probeBody)
	}
}

// A relay with no queried frame (every other provider, and probes) forwards the
// body byte for byte.
func TestAdmissionForwardsUntouchedWithoutQueried(t *testing.T) {
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	relay, err := newAdmission(upstream.URL, &http.Client{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	response, err := http.Post(relay.baseURL()+"/v1/messages", "application/json",
		strings.NewReader(restoredWireBody))
	if err != nil {
		t.Fatalf("generation: %v", err)
	}
	response.Body.Close()
	if string(received) != restoredWireBody {
		t.Fatal("body without a queried frame must forward untouched")
	}
}

// mustJSON quotes a string so it can be embedded as a JSON string literal.
func mustJSON(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
