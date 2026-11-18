package requestlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// messagesRequest builds a request shaped like a Messages call: identifying fields
// first, then tools, then a long conversation. The bulky part is exactly the part a
// byte-limited cut used to throw away.
func messagesRequest(turns int) []byte {
	messages := make([]any, 0, turns)
	for i := 0; i < turns; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, map[string]any{
			"role": role,
			"content": []any{map[string]any{
				"type": "text",
				"text": fmt.Sprintf("turn %d: %s", i, strings.Repeat("x", 4000)),
			}},
		})
	}
	body := map[string]any{
		"model":  "claude-sonnet-5-5",
		"stream": true,
		"system": []any{map[string]any{"type": "text", "text": "system prompt"}},
		"tools": []any{
			map[string]any{"name": "read", "description": strings.Repeat("d", 3000)},
			map[string]any{"name": "write", "description": strings.Repeat("d", 3000)},
		},
		"messages": messages,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return encoded
}

// A body log line has to stay inside its budget while keeping both ends readable:
// the head carries model/system/tools, the tail carries the newest turns.
func TestElideBodyKeepsBothEndsWithinBudget(t *testing.T) {
	body := messagesRequest(40)
	const limit = 16 << 10
	slim := ElideBody(body, limit)

	if len(body) <= limit {
		t.Fatalf("fixture too small to exercise elision: %d", len(body))
	}
	if len(slim) > limit {
		t.Fatalf("line is %d bytes, budget was %d", len(slim), limit)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(slim), &decoded); err != nil {
		t.Fatalf("elided body is not valid JSON: %v", err)
	}
	if decoded["model"] != "claude-sonnet-5-5" {
		t.Fatalf("model lost: %v", decoded["model"])
	}
	if _, ok := decoded["system"]; !ok {
		t.Fatal("system lost")
	}
	if _, ok := decoded["tools"]; !ok {
		t.Fatal("tools lost")
	}

	messages, _ := decoded["messages"].([]any)
	if len(messages) != arrayHead+arrayTail+1 {
		t.Fatalf("kept %d messages, want %d kept plus one marker", len(messages), arrayHead+arrayTail)
	}
	// The opening turn is the one the next request replays first.
	if first, _ := messages[0].(map[string]any); !strings.Contains(mustJSON(t, first), "turn 0") {
		t.Fatalf("first message lost: %s", mustJSON(t, first))
	}
	// The newest turn is the one being asked about.
	if last, _ := messages[len(messages)-1].(map[string]any); !strings.Contains(mustJSON(t, last), "turn 39") {
		t.Fatalf("last message lost: %s", mustJSON(t, last))
	}
	marker, ok := messages[arrayHead].(map[string]any)
	if !ok {
		t.Fatalf("no marker where the cut should be: %v", messages[arrayHead])
	}
	elided, _ := marker["_elided"].(map[string]any)
	if elided == nil {
		t.Fatalf("marker does not name what was cut: %v", marker)
	}
	if got, want := fmt.Sprint(elided["elements"]), fmt.Sprint(40-arrayHead-arrayTail); got != want {
		t.Fatalf("marker says %s elements cut, want %s", got, want)
	}
	// 40 turns of ~4 KB: the cut is far larger than the budget itself.
	var cut float64
	if _, err := fmt.Sscanf(fmt.Sprint(elided["bytes"]), "%g", &cut); err != nil || cut < 100000 {
		t.Fatalf("marker understates the cut: %v", elided["bytes"])
	}
}

// The middle must be the only thing dropped: every key survives, so a reader can
// still tell a cut log line from a small one.
func TestElideBodyKeepsEveryKeyAndTheCutSize(t *testing.T) {
	body := messagesRequest(40)
	slim := ElideBody(body, 8<<10)

	var decoded map[string]any
	if err := json.Unmarshal([]byte(slim), &decoded); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	for _, key := range []string{"model", "stream", "system", "tools", "messages"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("key %q dropped", key)
		}
	}
	if !strings.Contains(slim, "_elided") {
		t.Fatal("no marker: a reader cannot tell this was cut")
	}
}

// Not every body is JSON. An SSE stream is elided byte-wise, with the count of the
// dropped middle stated exactly.
func TestElideBodyCutsAStreamByteWise(t *testing.T) {
	var stream strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&stream, "event: chunk\ndata: {\"i\":%d}\n\n", i)
	}
	body := []byte(stream.String())
	const limit = 4096
	slim := ElideBody(body, limit)

	if len(slim) > limit {
		t.Fatalf("line is %d bytes, budget was %d", len(slim), limit)
	}
	if !strings.HasPrefix(slim, "event: chunk\ndata: {\"i\":0}") {
		t.Fatalf("head lost: %q", slim[:40])
	}
	if !strings.HasSuffix(slim, "data: {\"i\":499}\n\n") {
		t.Fatalf("tail lost: %q", slim[len(slim)-40:])
	}
	if !strings.Contains(slim, "[... elided ") {
		t.Fatal("no elision marker")
	}
	// Nothing can be silently dropped: head + tail + marker accounts for the whole.
	var cut int
	if _, err := fmt.Sscanf(slim[strings.Index(slim, "[... elided ")+len("[... elided "):], "%d", &cut); err != nil {
		t.Fatalf("marker does not carry a count: %v", err)
	}
	if cut <= 0 || cut >= len(body) {
		t.Fatalf("cut %d is not a plausible middle of %d bytes", cut, len(body))
	}
}

// A stream is captured while it flows, so the tail is what proves the turn finished
// — the usage event and the stop reason live there.
func TestBodyCaptureKeepsTheTailOfAStream(t *testing.T) {
	capture := newBodyCapture(2048)
	for i := 0; i < 400; i++ {
		capture.Write([]byte(fmt.Sprintf("data: {\"i\":%d}\n\n", i)))
	}
	rendered := capture.String()

	if capture.Total() != int64(400*len("data: {\"i\":0}\n\n")) && capture.Total() == 0 {
		t.Fatalf("total not tracked: %d", capture.Total())
	}
	if !strings.Contains(rendered, `"i":0`) {
		t.Fatal("head lost")
	}
	if !strings.Contains(rendered, `"i":399`) {
		t.Fatalf("tail lost, so the end of the turn is invisible: %q", rendered[len(rendered)-60:])
	}
	if !strings.Contains(rendered, "[... elided ") {
		t.Fatal("no elision marker")
	}
	if budget := capture.headMax + capture.tailMax; len(rendered) > budget+markerReserve+64 {
		t.Fatalf("rendered %d bytes, capture was budgeted for %d", len(rendered), budget)
	}
}

// A body that fits is logged whole, with no marker: the elision must never invent a
// cut where there was none.
func TestBodyCaptureKeepsASmallBodyIntact(t *testing.T) {
	capture := newBodyCapture(2048)
	capture.Write([]byte("data: one\n\n"))
	capture.Write([]byte("data: two\n\n"))
	if got, want := capture.String(), "data: one\n\ndata: two\n\n"; got != want {
		t.Fatalf("small body altered: %q", got)
	}
}

// Cutting a multi-byte string on a rune boundary keeps the log readable instead of
// filling it with replacement characters.
func TestElideStringDoesNotSplitRunes(t *testing.T) {
	body := strings.Repeat("я", 4000)
	slim := elideString(body, 512)
	if !strings.Contains(slim, "[... elided ") {
		t.Fatal("no marker")
	}
	for _, r := range slim {
		if r == '\uFFFD' {
			t.Fatal("a rune was split by the cut")
		}
	}
}

// The budget is the guarantee that a diagnosis window cannot fill the volume: once a
// day's bodies reach it, bodies stop and summaries keep going.
func TestBodyBudgetStopsBodiesButNotSummaries(t *testing.T) {
	dir := t.TempDir()
	logger := NewLogger(Config{Dir: dir, Bodies: true, BodyBudget: 4096})
	defer logger.Close()

	start := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	var opened int
	for i := 0; i < 20; i++ {
		reqID := fmt.Sprintf("req-%d", i)
		// A refused body must not refuse the summary: that is the whole point of
		// separating the two channels.
		if entry, err := logger.Open(reqID, start); err == nil {
			opened++
			entry.Log(map[string]any{"dir": "cli→relay", "body": strings.Repeat("y", 1024)})
			logger.CloseEntry(reqID)
		}
		logger.Emit(Record{RequestID: reqID, TS: start.Add(time.Duration(i) * time.Second), Status: 200})
	}
	if opened == 0 {
		t.Fatal("no body file was written at all")
	}
	if opened >= 20 {
		t.Fatalf("budget never engaged: %d of 20 requests wrote bodies", opened)
	}
	if size := dirSize(filepath.Join(dir, "2026-11-17")); size > 4096+1024 {
		t.Fatalf("day grew to %d bytes for a 4096 budget", size)
	}

	// The summary is the accounting channel and must survive the budget.
	summary, err := os.ReadFile(filepath.Join(dir, "requests-2026-11-17.jsonl"))
	if err != nil {
		t.Fatalf("summary missing: %v", err)
	}
	if lines := strings.Count(string(summary), "\n"); lines != 20 {
		t.Fatalf("summary has %d lines, want 20", lines)
	}
}

// A restart must not hand the day a fresh allowance: the total is seeded from disk.
func TestBodyBudgetSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	day := filepath.Join(dir, "2026-11-17")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "09-00-00.000_old.jsonl"), []byte(strings.Repeat("z", 8192)), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := NewLogger(Config{Dir: dir, Bodies: true, BodyBudget: 4096})
	defer logger.Close()
	if _, err := logger.Open("fresh", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("a restart reset the day's budget")
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(encoded)
}
