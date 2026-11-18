package requestlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A streamed Anthropic turn reports usage at the very end, which is exactly what
// the old first-8KB body log could never capture.
func TestUsageScannerAnthropicStream(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":42,"cache_read_input_tokens":1000}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9,"cache_creation_input_tokens":7}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

	scanner := NewUsageScanner()
	// Feed in awkward slices so line splitting is exercised.
	for _, chunk := range splitEvery(stream, 17) {
		scanner.Write([]byte(chunk))
	}
	scanner.Finish()

	input, output, cacheRead, cacheWrite := scanner.Result()
	if input != 42 || output != 9 || cacheRead != 1000 || cacheWrite != 7 {
		t.Fatalf("usage = %d/%d/%d/%d, want 42/9/1000/7", input, output, cacheRead, cacheWrite)
	}
}

func TestUsageScannerOpenAIChat(t *testing.T) {
	stream := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"usage":{"prompt_tokens":120,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":64}}}` + "\n\n" +
		"data: [DONE]\n\n"

	scanner := NewUsageScanner()
	scanner.Write([]byte(stream))
	scanner.Finish()

	input, output, cacheRead, cacheWrite := scanner.Result()
	if input != 120 || output != 30 || cacheRead != 64 || cacheWrite != 0 {
		t.Fatalf("usage = %d/%d/%d/%d, want 120/30/64/0", input, output, cacheRead, cacheWrite)
	}
}

func TestUsageScannerResponsesNested(t *testing.T) {
	event := `event: response.completed` + "\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":51,"output_tokens":8,"input_tokens_details":{"cached_tokens":5}}}}` + "\n\n"
	scanner := NewUsageScanner()
	scanner.Write([]byte(event))
	scanner.Finish()
	input, output, cacheRead, _ := scanner.Result()
	if input != 51 || output != 8 || cacheRead != 5 {
		t.Fatalf("usage = %d/%d/%d, want 51/8/5", input, output, cacheRead)
	}
}

// Garbage must never break a request: the scanner is a passenger, not a gate.
func TestUsageScannerIgnoresGarbage(t *testing.T) {
	scanner := NewUsageScanner()
	scanner.Write([]byte("mise WARN not activated\n"))
	scanner.Write([]byte(`data: {broken json usage` + "\n"))
	scanner.Write([]byte{0x00, 0xff, 0xfe, '\n'})
	scanner.Write([]byte(strings.Repeat("x", maxUsageLine+10)))
	scanner.Write([]byte("\n"))
	scanner.Finish()
	if in, out, _, _ := scanner.Result(); in != 0 || out != 0 {
		t.Fatalf("garbage produced usage %d/%d", in, out)
	}
}

// Non-streaming JSON (one line, no newline) must still be read.
func TestUsageScannerNonStreamingBody(t *testing.T) {
	scanner := NewUsageScanner()
	scanner.Write([]byte(`{"type":"message","content":[],"usage":{"input_tokens":7,"output_tokens":3}}`))
	scanner.Finish()
	if in, out, _, _ := scanner.Result(); in != 7 || out != 3 {
		t.Fatalf("usage = %d/%d, want 7/3", in, out)
	}
}

// A repeated body is the only visible trace of a client retry loop.
func TestRetryTrackerCountsRepeats(t *testing.T) {
	tracker := newRetryTracker(8, time.Minute)
	now := time.Now()

	attempt, retryOf := tracker.Observe("hash-a", "req-1", now)
	if attempt != 1 || retryOf != "" {
		t.Fatalf("first attempt = %d/%q, want 1/empty", attempt, retryOf)
	}
	attempt, retryOf = tracker.Observe("hash-a", "req-2", now.Add(time.Second))
	if attempt != 2 || retryOf != "req-1" {
		t.Fatalf("second attempt = %d/%q, want 2/req-1", attempt, retryOf)
	}
	attempt, _ = tracker.Observe("hash-a", "req-3", now.Add(2*time.Second))
	if attempt != 3 {
		t.Fatalf("third attempt = %d, want 3", attempt)
	}
	// A different body is a different request.
	if attempt, _ := tracker.Observe("hash-b", "req-4", now); attempt != 1 {
		t.Fatalf("different hash attempt = %d, want 1", attempt)
	}
	// After the window, the count restarts: a retry storm that stopped is over.
	if attempt, retryOf := tracker.Observe("hash-a", "req-5", now.Add(2*time.Minute)); attempt != 1 || retryOf != "" {
		t.Fatalf("after TTL = %d/%q, want 1/empty", attempt, retryOf)
	}
}

func TestRetryTrackerStaysBounded(t *testing.T) {
	tracker := newRetryTracker(4, time.Hour)
	now := time.Now()
	for i := 0; i < 50; i++ {
		tracker.Observe("hash-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "req", now)
	}
	if len(tracker.entries) > 4 {
		t.Fatalf("tracker holds %d entries, want at most 4", len(tracker.entries))
	}
}

// Probes are the noise that made 13% of logged requests phantom errors.
func TestIsProbe(t *testing.T) {
	probes := []struct{ method, path string }{
		{"GET", "/v1/models"},
		{"GET", "/props"},
		{"GET", "/api/tags"},
		{"GET", "/version"},
		{"GET", "/v1beta/models/gemini-2"},
		{"HEAD", "/health"},
	}
	for _, p := range probes {
		if !IsProbe(p.method, p.path) {
			t.Fatalf("%s %s should be a probe", p.method, p.path)
		}
	}
	notProbes := []struct{ method, path string }{
		{"POST", "/v1/messages"},
		{"POST", "/v1/chat/completions"},
		{"POST", "/v1/models"}, // a POST there is not a poll
		{"GET", "/v1/messages/123"},
	}
	for _, p := range notProbes {
		if IsProbe(p.method, p.path) {
			t.Fatalf("%s %s should not be a probe", p.method, p.path)
		}
	}
}

func newTestLogger(t *testing.T, cfg Config) *Logger {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	return NewLogger(cfg)
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("bad json line %q: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}

// One summary line per request, with the fields that used to be spread across
// four files.
func TestEmitWritesSummary(t *testing.T) {
	logger := newTestLogger(t, Config{})
	defer logger.Close()

	rec := Record{
		RequestID: "req-1", TS: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
		Method: "POST", Path: "/v1/messages", Model: "deepseek-v4-pro",
		ModelResolved: "deepseek-v4-pro", Provider: "deepseek",
		Status: 200, UpstreamCalled: true, LatencyMS: 1234, TTFBMS: 300,
		Stream: true, InputTokens: 42, OutputTokens: 9, Client: "claude-cli",
		Task: "chat", BodyHash: "abc",
	}
	logger.Emit(rec)

	lines := readLines(t, filepath.Join(logger.Config().Dir, "requests-2026-11-14.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("summary lines = %d, want 1", len(lines))
	}
	row := lines[0]
	if row["dir"] != "summary" {
		t.Fatalf("dir = %v", row["dir"])
	}
	for field, want := range map[string]any{
		"request_id": "req-1", "model": "deepseek-v4-pro", "provider": "deepseek",
		"status": float64(200), "upstream_called": true, "stream": true,
		"input_tokens": float64(42), "output_tokens": float64(9), "ttfb_ms": float64(300),
	} {
		if got := row[field]; got != want {
			t.Fatalf("%s = %v, want %v", field, got, want)
		}
	}
	if _, hasCost := row["cost_usd"]; hasCost {
		t.Fatal("cost must not be written: no price table exists")
	}
}

// Errors get their own file, and a probe never pollutes it.
func TestEmitSeparatesErrorsAndDropsProbes(t *testing.T) {
	logger := newTestLogger(t, Config{})
	defer logger.Close()
	dir := logger.Config().Dir

	logger.Emit(Record{RequestID: "ok", TS: time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC),
		Method: "POST", Path: "/v1/messages", Status: 200})
	logger.Emit(Record{RequestID: "bad", TS: time.Date(2026, 11, 14, 2, 0, 0, 0, time.UTC),
		Method: "POST", Path: "/v1/messages", Status: 403, ErrorCode: "oauth_not_allowed"})
	logger.Emit(Record{RequestID: "probe", TS: time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC),
		Method: "GET", Path: "/api/tags", Status: 401})

	summary := readLines(t, filepath.Join(dir, "requests-2026-11-14.jsonl"))
	if len(summary) != 2 {
		t.Fatalf("summary lines = %d, want 2 (probe excluded)", len(summary))
	}
	errorsOnly := readLines(t, filepath.Join(dir, "errors-2026-11-14.jsonl"))
	if len(errorsOnly) != 1 || errorsOnly[0]["request_id"] != "bad" {
		t.Fatalf("errors file = %v", errorsOnly)
	}
}

// Probes can be re-enabled, which is how a probe-driven 404 storm is diagnosed.
func TestEmitKeepsProbesWhenEnabled(t *testing.T) {
	logger := newTestLogger(t, Config{LogProbes: true})
	defer logger.Close()
	logger.Emit(Record{RequestID: "probe", TS: time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC),
		Method: "GET", Path: "/props", Status: 200})
	summary := readLines(t, filepath.Join(logger.Config().Dir, "requests-2026-11-14.jsonl"))
	if len(summary) != 1 {
		t.Fatalf("summary lines = %d, want 1", len(summary))
	}
	if summary[0]["probe"] != true {
		t.Fatalf("probe flag missing: %v", summary[0])
	}
}

// The whole point of the index: "who spent what" as a query, not a file scan.
func TestIndexGroupsByClientAndModel(t *testing.T) {
	logger := newTestLogger(t, Config{Index: true})
	defer logger.Close()

	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	records := []Record{
		{RequestID: "1", TS: base, Method: "POST", Path: "/v1/messages", Client: "hermes",
			Model: "deepseek-v4-pro", Provider: "deepseek", Status: 200,
			UpstreamCalled: true, InputTokens: 100, OutputTokens: 10},
		{RequestID: "2", TS: base.Add(time.Minute), Method: "POST", Path: "/v1/messages", Client: "hermes",
			Model: "deepseek-v4-pro", Provider: "deepseek", Status: 200,
			UpstreamCalled: true, InputTokens: 200, OutputTokens: 20},
		{RequestID: "3", TS: base.Add(2 * time.Minute), Method: "POST", Path: "/v1/messages", Client: "curl",
			Model: "claude-sonnet-5-5", Provider: "claude", Status: 403},
	}
	for _, rec := range records {
		logger.Emit(rec)
	}

	byClient, err := logger.Stats(StatsQuery{Since: base.Add(-time.Hour), Until: base.Add(time.Hour), GroupBy: "client"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byClient.Rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(byClient.Rows), byClient.Rows)
	}
	var hermes StatsRow
	for _, row := range byClient.Rows {
		if row.Key == "hermes" {
			hermes = row
		}
	}
	if hermes.Requests != 2 || hermes.InputTokens != 300 || hermes.Errors != 0 {
		t.Fatalf("hermes row = %+v", hermes)
	}

	byModel, err := logger.Stats(StatsQuery{Since: base.Add(-time.Hour), Until: base.Add(time.Hour), GroupBy: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if byModel.Total.Requests != 3 || byModel.Total.Errors != 1 {
		t.Fatalf("totals = %+v", byModel.Total)
	}
}

// Grouping names reach SQL, so anything outside the whitelist must be refused.
func TestIndexRejectsUnknownGroup(t *testing.T) {
	logger := newTestLogger(t, Config{Index: true})
	defer logger.Close()
	if _, err := logger.Stats(StatsQuery{GroupBy: "1; DROP TABLE requests"}); err == nil {
		t.Fatal("unsupported group_by must be refused")
	}
	if _, err := logger.Stats(StatsQuery{GroupBy: "requests"}); err == nil {
		t.Fatal("requests is not a whitelisted group")
	}
	if ValidateGroup("client") == false || ValidateGroup("provider") == false {
		t.Fatal("known groups must validate")
	}
}

// Retention must actually shrink the volume: measured 250 MB/day, no TTL.
func TestRetentionCompressesAndDrops(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().AddDate(0, 0, -20)
	day := old.Format("2006-01-02")
	dayDir := filepath.Join(dir, day)
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := filepath.Join(dayDir, "10-00-00.000_abc.jsonl")
	if err := os.WriteFile(body, []byte(strings.Repeat("log line\n", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := filepath.Join(dir, "requests-"+day+".jsonl")
	if err := os.WriteFile(summary, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Retention 15 days: the 20-day-old directory is past the compression cutoff
	// but not yet past the drop threshold (twice the window).
	logger := newTestLogger(t, Config{Dir: dir, RetentionDays: 15})
	defer logger.Close()

	report, err := logger.ApplyRetention(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Compressed == 0 {
		t.Fatal("nothing was compressed")
	}
	if _, err := os.Stat(body + ".zst"); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if _, err := os.Stat(body); !os.IsNotExist(err) {
		t.Fatal("original body file should be gone")
	}
	if _, err := os.Stat(summary + ".zst"); err != nil {
		t.Fatal("summary should be compressed too")
	}

	// A day past twice the retention window is dropped entirely (already
	// compressed, so the directory is pure archive).
	report, err = logger.ApplyRetention(time.Now().AddDate(0, 0, 25))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dayDir); !os.IsNotExist(err) {
		t.Fatalf("expired day directory still present (report %+v)", report)
	}
}

func splitEvery(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// Legacy bodies are truncated with a marker, so a strict parse fails; the model
// must still be recovered from the prefix or backfilled stats are empty.
func TestPeekLegacyBodyRecoversTruncated(t *testing.T) {
	body := `{"model":"claude-sonnet-5-5","stream":true,"messages":[{"role":"user","content":"x"` +
		"\n[... truncated ...]"
	model, stream := peekLegacyBody([]byte(body))
	if model != "claude-sonnet-5-5" {
		t.Fatalf("model = %q, want claude-sonnet-5-5", model)
	}
	if !stream {
		t.Fatal("stream flag lost")
	}
	// A complete body still parses the normal way.
	if m, s := peekLegacyBody([]byte(`{"model":"m1","stream":false}`)); m != "m1" || s {
		t.Fatalf("complete body = %q/%v", m, s)
	}
}

// Backfill must turn old per-request files into index rows.
func TestBackfillIndexesLegacyFiles(t *testing.T) {
	dir := t.TempDir()
	day := time.Now().Format("2006-01-02")
	dayDir := filepath.Join(dir, day)
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []map[string]any{
		{"dir": "user→proxy", "method": "POST", "path": "/v1/messages",
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"headers": map[string]string{"User-Agent": "hermes/1.0", "X-Hermes-Task": "chat"},
			"body":    `{"model":"deepseek-v4-pro","stream":true}`},
		{"dir": "proxy→upstream", "url": "https://api.deepseek.com/v1/messages",
			"ts": time.Now().UTC().Format(time.RFC3339Nano)},
		{"dir": "upstream→proxy", "status": 200,
			"headers": map[string]string{"x-ratelimit-limit-requests": "400"},
			"ts":      time.Now().UTC().Format(time.RFC3339Nano)},
		{"dir": "proxy→user", "status": 200, "ts": time.Now().UTC().Format(time.RFC3339Nano)},
	}
	var builder strings.Builder
	for _, line := range lines {
		encoded, _ := json.Marshal(line)
		builder.Write(encoded)
		builder.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dayDir, "10-00-00.000_legacyreq1.jsonl"), []byte(builder.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := newTestLogger(t, Config{Dir: dir, Index: true})
	defer logger.Close()

	from := time.Now().AddDate(0, 0, -2)
	report, err := logger.Backfill(from, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 1 || report.Files != 1 {
		t.Fatalf("report = %+v", report)
	}

	stats, err := logger.Stats(StatsQuery{Since: from, Until: time.Now().Add(time.Hour), GroupBy: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Rows) != 1 || stats.Rows[0].Key != "deepseek-v4-pro" {
		t.Fatalf("rows = %+v", stats.Rows)
	}
	byTask, err := logger.Stats(StatsQuery{Since: from, Until: time.Now().Add(time.Hour), GroupBy: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byTask.Rows) != 1 || byTask.Rows[0].Key != "chat" {
		t.Fatalf("task rows = %+v", byTask.Rows)
	}
}
