package anthropic

import (
	"strings"
	"testing"
)

// Dedicated assertions for every intentional deviation from the reference fixtures
// (see testdata/reference/DIFFERENCES.md). The golden tests skip these fixtures.

func convReq(t *testing.T, in string) (map[string]any, []string) {
	t.Helper()
	b, warns, err := ConvertResponsesRequestToAnthropic([]byte(in))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	v, _ := decodeJSON(b)
	return v.(map[string]any), warns
}

func TestInstructionsBecomeSystem(t *testing.T) {
	out, _ := convReq(t, `{"model":"m","instructions":"You are terse.","input":"hi"}`)
	if out["system"] != "You are terse." {
		t.Fatalf("system = %v", out["system"])
	}
	// Together with system messages from the input: instructions come first.
	out, _ = convReq(t, `{"model":"m","instructions":"INSTR","input":[
		{"type":"message","role":"system","content":[{"type":"input_text","text":"S1"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	blocks, ok := out["system"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("system = %#v", out["system"])
	}
	if blocks[0].(map[string]any)["text"] != "INSTR" || blocks[1].(map[string]any)["text"] != "S1" {
		t.Fatalf("system order wrong: %#v", blocks)
	}
}

func TestParallelToolCallsFalse(t *testing.T) {
	out, _ := convReq(t, `{"model":"m","input":"hi","parallel_tool_calls":false,
		"tools":[{"type":"function","name":"a","parameters":{"type":"object"}}]}`)
	tc := out["tool_choice"].(map[string]any)
	if tc["type"] != "auto" || tc["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice = %#v", tc)
	}
	// Combined with an explicit tool.
	out, _ = convReq(t, `{"model":"m","input":"hi","parallel_tool_calls":false,"tool_choice":"required",
		"tools":[{"type":"function","name":"a","parameters":{"type":"object"}}]}`)
	tc = out["tool_choice"].(map[string]any)
	if tc["type"] != "any" || tc["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice = %#v", tc)
	}
	// No tools: nothing to configure.
	out, _ = convReq(t, `{"model":"m","input":"hi","parallel_tool_calls":false}`)
	if _, has := out["tool_choice"]; has {
		t.Fatal("tool_choice without tools")
	}
}

func TestNonObjectToolArgumentsBecomeEmptyObject(t *testing.T) {
	out, warns := convReq(t, `{"model":"m","input":[
		{"type":"function_call","call_id":"c","name":"f","arguments":"[1,2]"},
		{"type":"function_call_output","call_id":"c","output":"ok"}]}`)
	msgs := out["messages"].([]any)
	tu := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if in, ok := tu["input"].(map[string]any); !ok || len(in) != 0 {
		t.Fatalf("input = %#v", tu["input"])
	}
	if len(warns) == 0 {
		t.Fatal("expected a warning")
	}
}

func TestFileDataURLPrefixStripped(t *testing.T) {
	out, _ := convReq(t, `{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,AAAA"}]}]}`)
	doc := out["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	src := doc["source"].(map[string]any)
	if src["data"] != "AAAA" || src["media_type"] != "application/pdf" || doc["title"] != "a.pdf" {
		t.Fatalf("doc = %#v", doc)
	}
}

func TestUnsupportedToolsAreDroppedNotFatal(t *testing.T) {
	out, warns := convReq(t, `{"model":"m","input":"hi","tools":[
		{"type":"local_shell"},{"type":"file_search"},
		{"type":"function","name":"keep","parameters":{"type":"object"}}]}`)
	tools := out["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "keep" {
		t.Fatalf("tools = %#v", tools)
	}
	if len(warns) != 2 {
		t.Fatalf("warnings = %v", warns)
	}
	out, _ = convReq(t, `{"model":"m","input":"hi","tools":[{"type":"custom","name":"x"}]}`)
	if _, has := out["tools"]; has {
		t.Fatal("empty tools array must not be sent")
	}
}

func TestNestedFunctionTool(t *testing.T) {
	out, _ := convReq(t, `{"model":"m","input":"hi","tools":[{"type":"function",
		"function":{"name":"nf","description":"d","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"nf"}}}`)
	tool := out["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "nf" || tool["description"] != "d" {
		t.Fatalf("tool = %#v", tool)
	}
	if tc := out["tool_choice"].(map[string]any); tc["type"] != "tool" || tc["name"] != "nf" {
		t.Fatalf("tool_choice = %#v", tc)
	}
}

func TestUserBecomesMetadata(t *testing.T) {
	out, _ := convReq(t, `{"model":"m","input":"hi","user":"u1"}`)
	if md := out["metadata"].(map[string]any); md["user_id"] != "u1" {
		t.Fatalf("metadata = %#v", md)
	}
}

func TestEmptyTextSkippedAndRefusalReplayed(t *testing.T) {
	out, _ := convReq(t, `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":""},{"type":"input_text","text":"x"}]},
		{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]},
		{"type":"message","role":"user","content":"n"}]}`)
	msgs := out["messages"].([]any)
	first := msgs[0].(map[string]any)["content"].([]any)
	if len(first) != 1 || first[0].(map[string]any)["text"] != "x" {
		t.Fatalf("first = %#v", first)
	}
	asst := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if asst["text"] != "no" {
		t.Fatalf("assistant = %#v", asst)
	}
}

func TestRequestErrors(t *testing.T) {
	for name, in := range map[string]string{
		"no input":      `{"model":"m"}`,
		"zero tokens":   `{"model":"m","input":"x","max_output_tokens":0}`,
		"not an object": `[1]`,
		"bad json":      `{`,
	} {
		if _, _, err := ConvertResponsesRequestToAnthropic([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestReasoningSummaryNone(t *testing.T) {
	out, _ := convReq(t, `{"model":"m","input":"hi","reasoning":{"effort":"medium","summary":"none"}}`)
	th := out["thinking"].(map[string]any)
	if th["type"] != "adaptive" || th["display"] != "omitted" {
		t.Fatalf("thinking = %#v", th)
	}
}

func TestNamespacedFunctionCallRoundtrip(t *testing.T) {
	out, _ := convReq(t, `{"model":"m","input":[
		{"type":"function_call","call_id":"c","name":"get","namespace":"mcp__srv__","arguments":"{}"},
		{"type":"function_call_output","call_id":"c","output":"ok"}],
		"tools":[{"type":"namespace","name":"mcp__srv__","tools":[{"type":"function","name":"get","parameters":{"type":"object"}}]}]}`)
	tools := out["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "mcp__srv__--get" {
		t.Fatalf("tool = %#v", tools[0])
	}
	tu := out["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tu["name"] != "mcp__srv__--get" {
		t.Fatalf("tool_use name = %v", tu["name"])
	}
	// Flat (adapter-expanded) tools keep their plain name even when the history
	// carries a namespace.
	out, _ = convReq(t, `{"model":"m","input":[
		{"type":"function_call","call_id":"c","name":"get","namespace":"mcp__srv__","arguments":"{}"},
		{"type":"function_call_output","call_id":"c","output":"ok"}],
		"tools":[{"type":"function","name":"get","parameters":{"type":"object"}}]}`)
	tu = out["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tu["name"] != "get" {
		t.Fatalf("flat tool_use name = %v", tu["name"])
	}
}

// ---- response / stream deviations ----

func TestResponseStopSequenceIsCompleted(t *testing.T) {
	b, err := ConvertAnthropicResponseToResponses([]byte(`{"id":"m","type":"message","role":"assistant","model":"x",
		"content":[{"type":"text","text":"a"}],"stop_reason":"stop_sequence","usage":{"input_tokens":1,"output_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := decodeJSON(b)
	if v.(map[string]any)["status"] != "completed" {
		t.Fatalf("status = %v", v.(map[string]any)["status"])
	}
}

func TestResponseUnsignedThinkingKept(t *testing.T) {
	b, _ := ConvertAnthropicResponseToResponses([]byte(`{"id":"m","type":"message","role":"assistant","model":"x",
		"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"a"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":1,"output_tokens":1}}`))
	v, _ := decodeJSON(b)
	out := v.(map[string]any)["output"].([]any)
	if len(out) != 2 || out[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("output = %#v", out)
	}
	if _, has := out[0].(map[string]any)["encrypted_content"]; has {
		t.Fatal("no signature, so no encrypted_content")
	}
}

func TestResponseIDsAreDeterministicAndCreatedAtSet(t *testing.T) {
	in := []byte(`{"id":"msg_x","type":"message","role":"assistant","model":"x",
		"content":[{"type":"text","text":"a"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	a, _ := ConvertAnthropicResponseToResponses(in)
	b, _ := ConvertAnthropicResponseToResponses(in)
	if string(a) != string(b) {
		t.Fatal("conversion is not deterministic")
	}
	v, _ := decodeJSON(a)
	if n, _ := toInt(v.(map[string]any)["created_at"]); n <= 0 {
		t.Fatalf("created_at = %v", v.(map[string]any)["created_at"])
	}
}

func TestResponseErrorObjectRejected(t *testing.T) {
	if _, err := ConvertAnthropicResponseToResponses([]byte(`{"type":"error","error":{"type":"x","message":"y"}}`)); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ConvertAnthropicResponseToResponses([]byte(`not json`)); err == nil {
		t.Fatal("expected error")
	}
}

func eventTypes(evs []map[string]any) []string {
	var r []string
	for _, e := range evs {
		r = append(r, strings.TrimPrefix(e["type"].(string), "response."))
	}
	return r
}

func TestStreamReasoningIsEmittedWithSignature(t *testing.T) {
	events := []any{
		map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "model": "x", "usage": map[string]any{"input_tokens": 3, "output_tokens": 1}}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": ""}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "ab"}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "S1"}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "S2"}},
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": ""}},
		map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "Hi"}},
		map[string]any{"type": "content_block_stop", "index": 1},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 4}},
		map[string]any{"type": "message_stop"},
	}
	evs := runStream(t, events)
	var done map[string]any
	for _, e := range evs {
		if e["type"] == "response.output_item.done" {
			if it := e["item"].(map[string]any); it["type"] == "reasoning" {
				done = it
			}
		}
	}
	if done == nil {
		t.Fatalf("no reasoning item.done in %v", eventTypes(evs))
	}
	if done["encrypted_content"] != "S1S2" {
		t.Fatalf("encrypted_content = %v", done["encrypted_content"])
	}
	sum := done["summary"].([]any)[0].(map[string]any)
	if sum["text"] != "ab" {
		t.Fatalf("summary = %#v", sum)
	}
	last := evs[len(evs)-1]
	if last["type"] != "response.completed" {
		t.Fatalf("last = %v", last["type"])
	}
	out := last["response"].(map[string]any)["output"].([]any)
	if len(out) != 2 || out[0].(map[string]any)["type"] != "reasoning" || out[1].(map[string]any)["type"] != "message" {
		t.Fatalf("final output = %#v", out)
	}
	// sequence numbers are strictly increasing from 0.
	for i, e := range evs {
		if n, _ := toInt(e["sequence_number"]); n != i {
			t.Fatalf("event %d has sequence_number %d", i, n)
		}
	}
}

func TestStreamToleratesMissingInputTokensAndUnknownEvents(t *testing.T) {
	events := []any{
		map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "model": "x"}},
		map[string]any{"type": "x_unknown", "foo": 1},
		map[string]any{"type": "ping"},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "a"}},
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 3}},
		map[string]any{"type": "message_stop"},
	}
	evs := runStream(t, events)
	last := evs[len(evs)-1]
	if last["type"] != "response.completed" {
		t.Fatalf("last = %v (%v)", last["type"], eventTypes(evs))
	}
	u := last["response"].(map[string]any)["usage"].(map[string]any)
	if n, _ := toInt(u["output_tokens"]); n != 3 {
		t.Fatalf("usage = %#v", u)
	}
}

func TestStreamMaxTokensIsIncomplete(t *testing.T) {
	events := []any{
		map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "model": "x"}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "a"}},
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "max_tokens"}, "usage": map[string]any{"output_tokens": 3}},
		map[string]any{"type": "message_stop"},
	}
	evs := runStream(t, events)
	last := evs[len(evs)-1]
	if last["type"] != "response.incomplete" {
		t.Fatalf("last = %v", last["type"])
	}
	resp := last["response"].(map[string]any)
	if resp["status"] != "incomplete" || resp["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("response = %#v", resp)
	}
}

func TestStreamErrorAndTruncation(t *testing.T) {
	// error event => response.failed with the upstream error.
	evs := runStream(t, []any{
		map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "model": "x"}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "a"}},
		map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "Overloaded"}},
	})
	last := evs[len(evs)-1]
	if last["type"] != "response.failed" {
		t.Fatalf("last = %v", last["type"])
	}
	e := last["response"].(map[string]any)["error"].(map[string]any)
	if e["code"] != "overloaded_error" || e["message"] != "Overloaded" {
		t.Fatalf("error = %#v", e)
	}
	// Stream ends without any terminal event => failed, open block is closed.
	evs = runStream(t, []any{
		map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "model": "x"}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "a"}},
	})
	if evs[len(evs)-1]["type"] != "response.failed" {
		t.Fatalf("truncated stream must fail, got %v", eventTypes(evs))
	}
	var sawItemDone bool
	for _, e := range evs {
		if e["type"] == "response.output_item.done" {
			sawItemDone = true
		}
	}
	if !sawItemDone {
		t.Fatal("open block was not closed before failing")
	}
}

func TestStreamNamespaceAndEmptyArgs(t *testing.T) {
	evs := runStream(t, []any{
		map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "model": "x"}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "t1", "name": "mcp__srv__--get", "input": map[string]any{}}},
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 1}},
		map[string]any{"type": "message_stop"},
	})
	for _, e := range evs {
		if e["type"] == "response.output_item.done" {
			it := e["item"].(map[string]any)
			if it["name"] != "get" || it["namespace"] != "mcp__srv__" || it["arguments"] != "{}" || it["call_id"] != "t1" {
				t.Fatalf("item = %#v", it)
			}
			return
		}
	}
	t.Fatal("no item.done")
}

func TestStreamEventsAfterTerminalAreIgnored(t *testing.T) {
	conv := NewAnthropicToResponsesStream()
	conv.Feed([]byte(`{"type":"message_start","message":{"id":"m","model":"x"}}`))
	conv.Feed([]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))
	conv.Feed([]byte(`{"type":"message_stop"}`))
	if !conv.Done() {
		t.Fatal("not done")
	}
	if got := conv.Feed([]byte(`{"type":"message_stop"}`)); got != nil {
		t.Fatalf("events after terminal: %v", got)
	}
	if got := conv.Finish(); got != nil {
		t.Fatalf("Finish after terminal: %v", got)
	}
	if got := conv.Feed([]byte(`garbage`)); got != nil {
		t.Fatal("garbage must be ignored")
	}
}

// Regression (found by the Codex emulator against DeepSeek): a signed thinking
// block with EMPTY text comes back as reasoning with an empty summary. It must
// be replayed as `thinking`, never as `redacted_thinking`, which DeepSeek rejects.
func TestEmptySummaryReasoningReplaysAsThinking(t *testing.T) {
	out, _ := convReq(t, `{"model":"m","input":[
		{"type":"message","role":"user","content":"q"},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"SIG"},
		{"type":"message","role":"assistant","content":"a"},
		{"type":"message","role":"user","content":"n"}]}`)
	blk := out["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if blk["type"] != "thinking" || blk["signature"] != "SIG" || blk["thinking"] != "" {
		t.Fatalf("block = %#v", blk)
	}
}

func TestRedactedThinkingRoundtrip(t *testing.T) {
	b, _ := ConvertAnthropicResponseToResponses([]byte(`{"id":"m","type":"message","role":"assistant","model":"x",
		"content":[{"type":"redacted_thinking","data":"XX"},{"type":"text","text":"a"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":1,"output_tokens":1}}`))
	v, _ := decodeJSON(b)
	item := v.(map[string]any)["output"].([]any)[0].(map[string]any)
	enc := item["encrypted_content"].(string)
	req, _ := marshalNoEscape(map[string]any{"model": "m", "input": []any{
		map[string]any{"type": "message", "role": "user", "content": "q"},
		item,
		map[string]any{"type": "message", "role": "assistant", "content": "a"},
		map[string]any{"type": "message", "role": "user", "content": "n"}}})
	out, _ := convReq(t, string(req))
	blk := out["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if blk["type"] != "redacted_thinking" || blk["data"] != "XX" {
		t.Fatalf("enc=%q block = %#v", enc, blk)
	}
}
