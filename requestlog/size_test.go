package requestlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeDay drops a day directory with one body file of the given size, plus the
// summaries a real day would have.
func writeDay(t *testing.T, dir, day string, bodyBytes int) {
	t.Helper()
	dayDir := filepath.Join(dir, day)
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dayDir, "09-00-00.000_req.jsonl"), []byte(strings.Repeat("b", bodyBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"requests", "errors"} {
		path := filepath.Join(dir, prefix+"-"+day+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Repeat("s", 1024)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The size cap exists because age alone is not a bound: the day that overruns is
// usually not the oldest one, so what has to go is the oldest material still inside
// the age window.
func TestSizeCapEvictsOldestUntilItFits(t *testing.T) {
	dir := t.TempDir()
	days := []string{"2026-11-10", "2026-11-11", "2026-11-12", "2026-11-13"}
	for _, day := range days {
		writeDay(t, dir, day, 4096)
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	// 4 days × (4 KB body + 2 KB summaries) ≈ 24 KB; the cap allows two of them.
	logger := NewLogger(Config{Dir: dir, RetentionDays: 14, MaxBytes: 12 << 10})
	defer logger.Close()

	report, err := logger.ApplyRetention(now)
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if report.Evicted == 0 {
		t.Fatal("nothing evicted, yet the tree is over its cap")
	}
	if total := treeSize(dir); total > 12<<10 {
		t.Fatalf("tree is still %d bytes, cap was %d", total, 12<<10)
	}
	for _, gone := range []string{"2026-11-10", "2026-11-11"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Fatalf("%s survived, though it was the oldest material", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-11-13")); err != nil {
		t.Fatalf("the newest day was evicted: %v", err)
	}
}

// The day being written is never deleted: its files are open, and its summaries are
// what the current window is accounted from. If one day alone is over the cap, the
// cap loses — the alternative is deleting live data.
func TestSizeCapNeverDeletesToday(t *testing.T) {
	dir := t.TempDir()
	today := "2026-11-13"
	writeDay(t, dir, today, 64<<10)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	logger := NewLogger(Config{Dir: dir, RetentionDays: 14, MaxBytes: 1024})
	defer logger.Close()

	if _, err := logger.ApplyRetention(now); err != nil {
		t.Fatalf("retention: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, today)); err != nil {
		t.Fatalf("today's directory was evicted: %v", err)
	}
}

// Eviction is not hostage to the age window: with the age limit off, the cap alone
// still keeps the tree bounded.
func TestSizeCapWorksWithoutAnAgeLimit(t *testing.T) {
	dir := t.TempDir()
	for _, day := range []string{"2026-11-10", "2026-11-11", "2026-11-12"} {
		writeDay(t, dir, day, 4096)
	}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	logger := NewLogger(Config{Dir: dir, RetentionDays: 0, MaxBytes: 8 << 10})
	defer logger.Close()

	report, err := logger.ApplyRetention(now)
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if report.Evicted == 0 {
		t.Fatal("age limit off disabled the size cap as well")
	}
	if total := treeSize(dir); total > 8<<10+2<<10 {
		t.Fatalf("tree is %d bytes, cap was %d", total, 8<<10)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-11-10")); err == nil {
		t.Fatal("the oldest day survived")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-11-12")); err != nil {
		t.Fatalf("the newest day was evicted: %v", err)
	}
}

// A compressed summary keeps its date: age and eviction both read it, so a .zst that
// stopped being recognised would live forever.
func TestCompressedSummaryKeepsItsDate(t *testing.T) {
	for _, name := range []string{
		"requests-2026-11-12.jsonl",
		"requests-2026-11-12.jsonl.zst",
		"errors-2026-11-12.jsonl.zst",
	} {
		if got := dayFromSummaryName(name); got != "2026-11-12" {
			t.Fatalf("%s → %q", name, got)
		}
	}
	if got := dayFromSummaryName("index.sqlite"); got != "" {
		t.Fatalf("index.sqlite treated as a summary: %q", got)
	}
}

// The whole point of the cap: it has to be reachable through configuration, and an
// unparsable value must not read as "no limit".
func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"20G":         20 << 30,
		"20GB":        20 << 30,
		"20480M":      20480 << 20,
		"10737418240": 10737418240,
		"1.5G":        1610612736,
		"512MiB":      512 << 20,
		"":            20 << 30,
		"nonsense":    20 << 30,
		"-5G":         20 << 30,
	}
	for raw, want := range cases {
		if got := ParseSize(raw, 20<<30); got != want {
			t.Fatalf("ParseSize(%q) = %d, want %d", raw, got, want)
		}
	}
}

// The daily pass is what the user asked for; "24" is how it is written by hand.
func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"24":    24 * time.Hour,
		"24h":   24 * time.Hour,
		"6h30m": 6*time.Hour + 30*time.Minute,
		"":      24 * time.Hour,
		"junk":  24 * time.Hour,
	}
	for raw, want := range cases {
		if got := ParseDuration(raw, 24*time.Hour); got != want {
			t.Fatalf("ParseDuration(%q) = %v, want %v", raw, got, want)
		}
	}
}
