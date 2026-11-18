package providers

// Check against the bodies captured from the wire during the cache investigation.
// Run with REAL_BODY_DIR=<dir with cli→relay jsonl files>. Skipped otherwise.
//
// The captured material is only the opening turn of a conversation (the subscription
// window closed before a tool round completed), so the second request is reconstructed
// in the layout the reference fixtures pin: the same first turn replayed bare, one tool
// round, native's per-request date message after it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRealWireBodyKeepsItsContentAndCachesTheHostFrame(t *testing.T) {
	dir := os.Getenv("REAL_BODY_DIR")
	if dir == "" {
		t.Skip("REAL_BODY_DIR not set")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Skipf("no captured bodies in %s", dir)
	}

	var checked int
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range splitJSONLines(raw) {
			var row map[string]any
			if json.Unmarshal(line, &row) != nil || row["dir"] != "cli→relay" {
				continue
			}
			body, _ := row["body"].(string)
			if body == "" {
				continue
			}
			checked++
			reportRealBody(t, filepath.Base(file), []byte(body))
		}
	}
	if checked == 0 {
		t.Skipf("no cli→relay bodies in %s", dir)
	}
	t.Logf("checked %d captured bodies", checked)
}

func reportRealBody(t *testing.T, name string, native []byte) {
	t.Helper()
	body := decodeBody(t, native)
	messages, _ := body["messages"].([]any)
	if len(messages) < 2 {
		t.Fatalf("%s: unexpected shape, %d messages", name, len(messages))
	}
	first, _ := messages[0].(map[string]any)
	content, _ := first["content"].([]any)
	if len(content) < 2 {
		t.Fatalf("%s: the opening turn carries %d blocks", name, len(content))
	}
	// The host's frame is what the client asked about: everything native prepended sits
	// ahead of it in the same message.
	queried := []any{content[len(content)-1]}
	hostFrame := canonical(t, content[len(content)-1].(map[string]any))

	total := 0
	for _, raw := range messages {
		if message, ok := raw.(map[string]any); ok {
			if blocks, ok := message["content"].([]any); ok {
				total += len(blocks)
			}
		}
	}
	before := portedMarks(t, native)
	beforeCount := markCount(t, native)

	patched := pinMessageBreakpoint(native, queried)
	after := decodeBody(t, patched)

	afterTotal := 0
	afterMessages, _ := after["messages"].([]any)
	for _, raw := range afterMessages {
		if message, ok := raw.(map[string]any); ok {
			if blocks, ok := message["content"].([]any); ok {
				afterTotal += len(blocks)
			}
		}
	}
	if afterTotal != total {
		t.Fatalf("%s: block count changed %d → %d", name, total, afterTotal)
	}
	for key := range body {
		if key == "messages" {
			continue
		}
		if !reflect.DeepEqual(plainValue(after[key]), plainValue(body[key])) {
			t.Fatalf("%s: %q changed", name, key)
		}
	}
	afterMessages, _ = after["messages"].([]any)
	afterFirst, _ := afterMessages[0].(map[string]any)
	afterContent, _ := afterFirst["content"].([]any)
	if got := canonical(t, afterContent[0].(map[string]any)); got != hostFrame {
		t.Fatalf("%s: the host frame is not first after the relay\n  got : %s\n  want: %s", name, got, hostFrame)
	}

	marks := portedMarks(t, patched)
	if len(marks) != 1 {
		t.Fatalf("%s: message marks after the relay = %v, want exactly one", name, marks)
	}
	if got, want := markCount(t, patched), beforeCount; got != want {
		t.Fatalf("%s: breakpoints %d → %d, the count must not change", name, beforeCount, got)
	}

	// The second request, in the reference layout: this turn replayed bare, one tool
	// round, native's date message last.
	next := realSecondTurn(hostFrame)

	// What the relay changes, measured on the real body: before, the cached span starts
	// with the context native prepends (which never recurs); after, with the host frame.
	beforePrefix := cachedPrefix(t, native)
	afterPrefix := cachedPrefix(t, patched)
	nextBlocks := replayedBlocks(t, next)

	recurs := func(prefix []string) bool {
		return len(prefix) > 0 && len(nextBlocks) >= len(prefix) &&
			reflect.DeepEqual(nextBlocks[:len(prefix)], prefix)
	}
	if recurs(beforePrefix) {
		t.Fatalf("%s: the uncorrected body already recurs — the fixture no longer shows the defect", name)
	}
	if !recurs(afterPrefix) {
		t.Fatalf("%s: the corrected body still does not recur", name)
	}
	t.Logf("%s: %d blocks, %d breakpoints | cached span before=%d blocks (recur=%v) after=%d blocks (recur=%v) | marks before=%v after=%v",
		name, total, beforeCount, len(beforePrefix), recurs(beforePrefix),
		len(afterPrefix), recurs(afterPrefix), before, marks)
}

// realSecondTurn reconstructs what the reference fixtures say the CLI sends next: the
// opening turn without native's prepended context, one tool round, then the date.
func realSecondTurn(hostFrameJSON string) []byte {
	frame := map[string]any{}
	if err := json.Unmarshal([]byte(hostFrameJSON), &frame); err != nil {
		panic(err)
	}
	messages := []any{
		map[string]any{"role": "user", "content": []any{frame}},
		map[string]any{"role": "system", "content": "Primary working directory: /tmp/claude-work-2"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "probe", "input": map[string]any{}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "out"},
		}},
		map[string]any{"role": "system", "content": []any{map[string]any{
			"type": "text", "text": "Today's date is 2026-11-17.", "cache_control": mark(),
		}}},
	}
	encoded, err := json.Marshal(map[string]any{"model": "m", "messages": messages})
	if err != nil {
		panic(err)
	}
	return encoded
}

func splitJSONLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				out = append(out, raw[start:i])
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}

var _ = fmt.Sprintf
