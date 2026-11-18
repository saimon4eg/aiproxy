package providers

// The invariant this transport exists for: the prefix one request caches is the
// prefix the next request replays, byte for byte. If it is not, the entry is written
// and never read, and the whole conversation is charged the cache-creation rate on
// every turn.
//
// The bodies below are the reference transport's own fixtures — the message layouts
// the CLI actually produces — ported test for test from the Python relay this
// transport replaces. What they pin, and what a hand-made fixture would miss:
//
//   - on the opening request native puts its per-request context AHEAD of the frame
//     the client queried, and on every later request replays that turn WITHOUT it,
//     so the first turn has to be put in the order the later requests use;
//   - the leading text of the newest turn recurs only as far as it equals the frame
//     the client queried, and native's reminder appended to the last tool_result is
//     what ends that span;
//   - from the second request on Claude Code marks two message blocks, and the one
//     on its trailing per-request context message never recurs.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const portedReminder = "\n<system-reminder>userEmail: user@example.com</system-reminder>"

func mark() map[string]any { return map[string]any{"type": "ephemeral"} }

// ── the opening turn: native's context ahead of the queried frame ──────────

var (
	portedEnv   = "Primary working directory: /tmp/claude-work-501\nPlatform: darwin"
	portedFrame = map[string]any{"type": "text",
		"text": "Use the terminal tool to run `echo alpha`." + strings.Repeat(" source pack", 200)}
	portedReminderBlock = map[string]any{"type": "text", "text": "<system-reminder>\n" +
		"As you answer the user's questions, you can use the following context:\n" +
		"# userEmail\nThe user's email address is a@example.invalid.\n</system-reminder>\n"}
)

// portedCall1 is native's opening request: the reminder ahead of the host frame, the
// message-level marker on the trailing environment/date message.
func portedCall1() []any {
	return []any{
		map[string]any{"role": "user", "content": []any{portedReminderBlock, portedFrame}},
		map[string]any{"role": "system", "content": []any{map[string]any{
			"type": "text", "text": portedEnv + "\n\nToday's date is 2026-09-30.",
			"cache_control": mark(),
		}}},
	}
}

// portedCall2 is native's second request: the same first turn replayed BARE, one tool
// round, the date after it.
func portedCall2() []any {
	return []any{
		map[string]any{"role": "user", "content": []any{portedFrame}},
		map[string]any{"role": "system", "content": portedEnv},
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": "t1", "name": "probe",
			"input": map[string]any{"command": "echo alpha"},
		}}},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "t1",
			"content": "alpha\n\n" + portedReminderBlock["text"].(string),
		}}},
		map[string]any{"role": "system", "content": []any{map[string]any{
			"type": "text", "text": "Today's date is 2026-09-30.", "cache_control": mark(),
		}}},
	}
}

// cachedPrefix is what the API stores for a request: every message block through the
// one carrying the marker, compared without the directive itself.
func cachedPrefix(t *testing.T, payload []byte) []string {
	t.Helper()
	out := replayedBlocks(t, payload)
	full := allBlockRefs(t, payload)
	for i, ref := range full {
		if _, has := ref.block["cache_control"]; has {
			return out[:i+1]
		}
	}
	return nil
}

// replayedBlocks is the request's message blocks in wire order, directives stripped —
// the sequence a later request has to reproduce from its start.
func replayedBlocks(t *testing.T, payload []byte) []string {
	t.Helper()
	refs := allBlockRefs(t, payload)
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		role, _ := ref.message.(string)
		out = append(out, role+"\x00"+canonical(t, ref.block))
	}
	return out
}

type portedRef struct {
	message any
	index   int
	block   map[string]any
}

func allBlockRefs(t *testing.T, payload []byte) []portedRef {
	t.Helper()
	body := decodeBody(t, payload)
	messages, _ := body["messages"].([]any)
	refs := make([]portedRef, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := message["role"]
		switch content := message["content"].(type) {
		case string:
			refs = append(refs, portedRef{message: role, block: map[string]any{"type": "text", "text": content}})
		case []any:
			for i, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				refs = append(refs, portedRef{message: role, index: i, block: block})
			}
		}
	}
	return refs
}

// canonical renders a block without its cache directive.
func canonical(t *testing.T, block map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(plainValue(block))
	if err != nil {
		t.Fatalf("encode block: %v", err)
	}
	return string(encoded)
}

// plainValue drops every cache directive, so two blocks compare by content.
func plainValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if key == "cache_control" {
				continue
			}
			out[key] = plainValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = plainValue(item)
		}
		return out
	default:
		return value
	}
}

// The whole cached span of call #1 must recur, block for block, at the head of call
// #2 — and it has to cover the host's first turn, the part that was being re-written.
func TestOpeningTurnCachesAPrefixTheSecondRequestReplays(t *testing.T) {
	queried := []any{portedFrame}
	first := pinMessageBreakpoint(marshalMessages(t, portedCall1()), queried)
	second := pinMessageBreakpoint(marshalMessages(t, portedCall2()), []any{map[string]any{
		"type": "tool_result", "tool_use_id": "t1", "content": "alpha",
	}})

	prefix := cachedPrefix(t, first)
	if prefix == nil {
		t.Fatal("the opening request caches nothing")
	}
	replayed := replayedBlocks(t, second)
	if len(replayed) < len(prefix) {
		t.Fatalf("second request replays %d blocks, the cached span is %d", len(replayed), len(prefix))
	}
	if !reflect.DeepEqual(replayed[:len(prefix)], prefix) {
		t.Fatalf("the cached span does not recur\n  cached  : %v\n  replayed: %v", prefix, replayed[:len(prefix)])
	}
	last := prefix[len(prefix)-1]
	if !strings.Contains(last, canonical(t, portedFrame)) {
		t.Fatalf("the cached span stops before the host's first turn: %s", last)
	}
}

// Every block of the opening turn is still forwarded — only the marker moves — and
// the host frame now precedes native's context, the order every later request uses.
func TestOpeningTurnKeepsEveryBlockAndMovesOnlyTheMarker(t *testing.T) {
	body := decodeBody(t, pinMessageBreakpoint(marshalMessages(t, portedCall1()), []any{portedFrame}))
	messages, _ := body["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	content, _ := first["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("block count changed: %d", len(content))
	}
	if got, want := canonical(t, content[0].(map[string]any)), canonical(t, portedFrame); got != want {
		t.Fatalf("first block is not the host frame\n  got : %s\n  want: %s", got, want)
	}
	if got, want := canonical(t, content[1].(map[string]any)), canonical(t, portedReminderBlock); got != want {
		t.Fatalf("native's context was not preserved\n  got : %s\n  want: %s", got, want)
	}
	if _, has := content[0].(map[string]any)["cache_control"]; !has {
		t.Fatal("the marker did not land on the host frame")
	}
	if got := messageMarkCount(t, body); got != 1 {
		t.Fatalf("message-level marks = %d, want 1", got)
	}
	trailing, _ := messages[1].(map[string]any)["content"].([]any)
	text, _ := trailing[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, portedEnv) {
		t.Fatalf("native's environment message changed: %q", text)
	}
}

// The first turn is only reordered when the queried frame can be identified without
// ambiguity; otherwise the body forwards exactly as native built it.
func TestOpeningTurnIsOnlyReorderedWhenTheFrameIsUnambiguous(t *testing.T) {
	cases := map[string][]any{
		"history present": append(portedCall2()[:3],
			map[string]any{"role": "user", "content": []any{portedReminderBlock, portedFrame}}),
		"frame twice":  []any{map[string]any{"role": "user", "content": []any{portedReminderBlock, portedFrame, portedFrame}}},
		"media first":  []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="}}, portedFrame}}},
		"frame absent": []any{map[string]any{"role": "user", "content": []any{portedReminderBlock, map[string]any{"type": "text", "text": "rewritten"}}}},
	}
	for name, messages := range cases {
		t.Run(name, func(t *testing.T) {
			last := messages[len(messages)-1].(map[string]any)
			content := last["content"].([]any)
			content[len(content)-1].(map[string]any)["cache_control"] = mark()
			before := marshalMessages(t, messages)

			after := pinMessageBreakpoint(before, []any{portedFrame})
			if got, want := replayedBlocks(t, after), replayedBlocks(t, before); !reflect.DeepEqual(got, want) {
				t.Fatalf("block order changed\n  got : %v\n  want: %v", got, want)
			}
		})
	}
}

// ── the tool rounds: two native marks, only one of them recurs ──────────────

func portedResults(n int) []any {
	out := make([]any, 0, 4)
	for k := 0; k < 4; k++ {
		out = append(out, map[string]any{
			"type": "tool_result", "tool_use_id": fmt.Sprintf("r%dc%d", n, k),
			"content": fmt.Sprintf("output %d.%d", n, k),
		})
	}
	return out
}

func portedAssistant(n int) map[string]any {
	content := []any{map[string]any{"type": "thinking", "thinking": fmt.Sprintf("plan %d", n), "signature": fmt.Sprintf("sig%d", n)}}
	for k := 0; k < 4; k++ {
		content = append(content, map[string]any{
			"type": "tool_use", "id": fmt.Sprintf("r%dc%d", n, k), "name": "mcp__hermes__terminal",
			"input": map[string]any{"command": fmt.Sprintf("echo %d.%d", n, k)},
		})
	}
	return map[string]any{"role": "assistant", "content": content}
}

// portedRounds is what native sends after `rounds` parallel rounds: older rounds
// replay as the host sent them, the newest carries native's reminder (optionally) and
// is followed by its per-request context message.
func portedRounds(rounds int, reminder bool) map[string]any {
	messages := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "run four probes, twice"},
	}}}
	for n := 0; n < rounds; n++ {
		messages = append(messages, portedAssistant(n), map[string]any{"role": "user", "content": portedResults(n)})
	}
	// Native's mark 1: the last tool_use of the turn. It recurs, so it stays.
	turn := messages[len(messages)-2].(map[string]any)
	turn["content"].([]any)[4].(map[string]any)["cache_control"] = mark()
	if reminder {
		newest := messages[len(messages)-1].(map[string]any)["content"].([]any)[3].(map[string]any)
		newest["content"] = newest["content"].(string) + portedReminder
	}
	messages = append(messages, map[string]any{"role": "system", "content": []any{map[string]any{
		"type": "text", "text": fmt.Sprintf("Today's date is 2026-10-04. Request %d.", rounds),
		"cache_control": mark(),
	}}})
	return map[string]any{
		"model": "claude-opus-5-5",
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc"},
			map[string]any{"type": "text", "text": "You are Claude Code. Host system prompt.", "cache_control": mark()},
		},
		"tools": []any{map[string]any{
			"name": "mcp__hermes__terminal", "description": "run",
			"input_schema": map[string]any{"type": "object"}, "cache_control": mark(),
		}},
		"messages": messages,
	}
}

// routed is what the relay does to one request: restore the queried frame, then pin.
func routed(t *testing.T, body map[string]any, queried []any) []byte {
	t.Helper()
	payload := marshalBody(t, body)
	restored, err := restoreQueriedTurn(payload, queried)
	if err != nil {
		t.Fatalf("restoration failed: %v", err)
	}
	return pinMessageBreakpoint(restored, queried)
}

// Native's mark on the last tool_use recurs and stays; the mark on its trailing
// per-request message moves onto the last tool result, which — restored to the host's
// frame — recurs. The count never grows, and the next request reads the entry.
func TestTwoNativeMarksKeepOneBreakpointTheNextRequestReads(t *testing.T) {
	for _, reminder := range []bool{false, true} {
		name := "context-only"
		if reminder {
			name = "reminder-in-last-result"
		}
		t.Run(name, func(t *testing.T) {
			nativeN := portedRounds(1, reminder)
			sentN := routed(t, nativeN, portedResults(0))
			sentN1 := routed(t, portedRounds(2, reminder), portedResults(1))

			if got, want := portedMarks(t, sentN), [][2]int{{1, 4}, {2, 3}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("marks after the relay = %v, want %v", got, want)
			}
			if got, want := portedMarks(t, sentN1), [][2]int{{3, 4}, {4, 3}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("marks on the next request = %v, want %v", got, want)
			}
			if got, want := markCount(t, sentN), markCount(t, marshalBody(t, nativeN)); got != want || want != 4 {
				t.Fatalf("breakpoints = %d (native %d), must stay at native's four", got, want)
			}
			// Only a directive moved: the inventory and the message content are native's.
			sentBody, nativeBody := decodeBody(t, sentN), marshalDecode(t, nativeN)
			if !reflect.DeepEqual(sentBody["system"], nativeBody["system"]) {
				t.Fatal("the system blocks changed")
			}
			if !reflect.DeepEqual(sentBody["tools"], nativeBody["tools"]) {
				t.Fatal("the tools inventory changed")
			}
			sentMessages, _ := sentBody["messages"].([]any)
			nativeMessages, _ := nativeBody["messages"].([]any)
			if got, want := canonical(t, sentMessages[3].(map[string]any)), canonical(t, nativeMessages[3].(map[string]any)); got != want {
				t.Fatalf("native's context message changed\n  got : %s\n  want: %s", got, want)
			}

			// The entry request N wrote is the prefix request N+1 starts from.
			last := portedMarks(t, sentN)[1]
			if got, want := prefixAt(t, sentN1, last), prefixAt(t, sentN, last); got != want {
				t.Fatalf("the next request does not replay the cached prefix\n  next: %s\n  sent: %s", got, want)
			}
		})
	}
}

// Restoration fails open: with native's reminder left in the last result the newest
// turn does not recur, so the only breakpoint kept is native's own on the tool_use —
// and it is read next time.
func TestUnrestorableResultFallsBackToNativeToolUseMark(t *testing.T) {
	native := portedRounds(1, true)
	sent := pinMessageBreakpoint(marshalBody(t, native), portedResults(0))

	if got, want := portedMarks(t, sent), [][2]int{{1, 4}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("marks = %v, want only native's own %v", got, want)
	}
	sentBody, nativeBody := decodeBody(t, sent), marshalDecode(t, native)
	if got, want := markCount(t, sent), markCount(t, sent); got != want {
		t.Fatalf("mark count changed: %d", got)
	}
	if !reflect.DeepEqual(plainValue(sentBody), plainValue(nativeBody)) {
		t.Fatal("content changed while falling back")
	}
	if got, want := prefixAt(t, marshalBody(t, portedRounds(2, true)), [2]int{1, 4}), prefixAt(t, sent, [2]int{1, 4}); got != want {
		t.Fatalf("native's own breakpoint is not replayed either\n  next: %s\n  sent: %s", got, want)
	}
}

// Two marks that both recur: nothing to drop, the payload forwards byte for byte.
func TestMarksInsideTheRecurringSpanAreLeftAlone(t *testing.T) {
	native := portedRounds(1, false)
	messages := native["messages"].([]any)
	native["messages"] = messages[:len(messages)-1] // no trailing context message
	last := native["messages"].([]any)[2].(map[string]any)["content"].([]any)[3].(map[string]any)
	last["cache_control"] = mark()
	raw := marshalBody(t, native)

	if got := pinMessageBreakpoint(raw, portedResults(0)); string(got) != string(raw) {
		t.Fatalf("a payload with nothing to move was rewritten\n  got : %s\n  want: %s", got, raw)
	}
}

// The invariant across a whole conversation: whatever turn N caches, turn N+1
// replays from its first block. This is the property the subscription bill depends
// on, and the one a single-request test cannot see.
func TestEveryRoundReplaysThePrefixThePreviousRoundCached(t *testing.T) {
	for round := 1; round <= 4; round++ {
		sent := routed(t, portedRounds(round, true), portedResults(round-1))
		marks := portedMarks(t, sent)
		if len(marks) == 0 {
			t.Fatalf("round %d caches nothing in the conversation", round)
		}
		at := marks[len(marks)-1]

		next := routed(t, portedRounds(round+1, true), portedResults(round))
		if got, want := prefixAt(t, next, at), prefixAt(t, sent, at); got != want {
			t.Fatalf("round %d: the next request does not replay the cached prefix", round)
		}
	}
}

// ── helpers ──

// prefixAt is the request's content through block (message, index) in the order the
// cache reads it — tools, then system, then messages — with directives removed.
func prefixAt(t *testing.T, payload []byte, at [2]int) string {
	t.Helper()
	body := decodeBody(t, payload)
	messages, _ := body["messages"].([]any)
	head := make([]any, 0, at[0]+1)
	head = append(head, messages[:at[0]]...)
	message, _ := messages[at[0]].(map[string]any)
	content, _ := message["content"].([]any)
	trimmed := make(map[string]any, len(message))
	for key, value := range message {
		if key != "content" {
			trimmed[key] = value
		}
	}
	trimmed["content"] = content[:at[1]+1]
	head = append(head, trimmed)

	encoded, err := json.Marshal(plainValue(map[string]any{
		"tools": body["tools"], "system": body["system"], "messages": head,
	}))
	if err != nil {
		t.Fatalf("encode prefix: %v", err)
	}
	return string(encoded)
}

func portedMarks(t *testing.T, payload []byte) [][2]int {
	t.Helper()
	body := decodeBody(t, payload)
	messages, _ := body["messages"].([]any)
	marks := make([][2]int, 0, 2)
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for j, item := range content {
			if block, ok := item.(map[string]any); ok {
				if _, has := block["cache_control"]; has {
					marks = append(marks, [2]int{i, j})
				}
			}
		}
	}
	return marks
}

func messageMarkCount(t *testing.T, body map[string]any) int {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"messages": body["messages"]})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return len(portedMarks(t, encoded))
}

// markCount counts every breakpoint a request spends, inventory included: Anthropic
// allows four.
func markCount(t *testing.T, payload []byte) int {
	t.Helper()
	body := decodeBody(t, payload)
	total := 0
	for _, key := range []string{"tools", "system"} {
		blocks, _ := body[key].([]any)
		for _, item := range blocks {
			if block, ok := item.(map[string]any); ok {
				if _, has := block["cache_control"]; has {
					total++
				}
			}
		}
	}
	return total + len(portedMarks(t, payload))
}

func marshalMessages(t *testing.T, messages []any) []byte {
	t.Helper()
	return marshalBody(t, map[string]any{"model": "m", "messages": messages})
}

func marshalBody(t *testing.T, body map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	return encoded
}

func marshalDecode(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	return decodeBody(t, marshalBody(t, body))
}
