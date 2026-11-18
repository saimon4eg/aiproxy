package requestlog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// contextKey is an unexported type used for context keys to avoid collisions.
type contextKey struct{}

var requestIDKey contextKey

// RequestIDFromContext extracts the request ID from context.
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// ContextWithRequestID stores the request ID in context.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// Entry is a single request log file: the four body-carrying lines. Only written
// when body logging is on.
type Entry struct {
	mu   sync.Mutex
	file *os.File
	path string
	// account reports how many bytes this entry wrote, so the logger can hold a day
	// to its body budget.
	account func(int)
}

// Log writes one JSONL line to the entry's file. Thread-safe.
func (e *Entry) Log(data map[string]interface{}) {
	if data == nil {
		data = make(map[string]interface{})
	}
	data["ts"] = time.Now().UTC().Format(time.RFC3339Nano)

	e.mu.Lock()
	defer e.mu.Unlock()

	line, err := json.Marshal(data)
	if err != nil {
		slog.Error("requestlog: marshal failed", "error", err, "path", e.path)
		return
	}
	line = append(line, '\n')
	n, err := e.file.Write(line)
	if err != nil {
		slog.Error("requestlog: write failed", "error", err, "path", e.path)
	}
	if e.account != nil && n > 0 {
		e.account(n)
	}
}

// Config controls what the request log records.
type Config struct {
	// Dir is the log root, relative to the working directory or absolute.
	Dir string
	// Bodies writes the four per-request body files. Expensive — 8 KB per line,
	// four lines per request — so it stays tied to debug mode.
	Bodies bool
	// LogProbes includes liveness/model-list polls in the summary and index.
	// They are noise: on a measured day 311 of 1938 requests were probes, and
	// they produced 176 404s plus 86 401s — 13% of all records, every one of
	// them a phantom error.
	LogProbes bool
	// RetentionDays compresses day directories to zstd and drops them after this
	// many days. Zero disables the age limit.
	RetentionDays int
	// RetentionInterval is how often the retention pass runs. Zero means daily.
	RetentionInterval time.Duration
	// MaxBytes caps the whole log tree, summaries included. When it is exceeded the
	// oldest day directories and summary files are deleted until the tree fits, so
	// the logs cannot fill the volume between two retention passes. Zero disables
	// the cap.
	MaxBytes int64
	// BodyBudget caps the body files written into one day directory, in bytes.
	// Once a day reaches it, further requests log their summary and skip their
	// bodies until the day rolls over. Zero disables the cap.
	BodyBudget int64
	// Index keeps summary lines in <Dir>/index.sqlite for grouped queries.
	Index bool
}

// Logger manages body entries and the always-on summary stream.
type Logger struct {
	cfg Config

	mu      sync.Mutex
	entries map[string]*Entry
	// dayBytes is how much body volume each day directory already holds, seeded
	// once per day from disk and then kept in step by Entry writes. It is what the
	// budget is checked against; without it a busy day could fill the volume and
	// nothing but retention — hours later — would notice.
	dayBytes  map[string]int64
	budgetHit map[string]bool

	summary *dailyWriter
	errors  *dailyWriter
	idx     *index
	retries *retryTracker
}

// NewLogger creates a Logger. Creates the output directory if needed.
func NewLogger(cfg Config) *Logger {
	if cfg.Dir == "" {
		cfg.Dir = "logs"
	}
	_ = os.MkdirAll(cfg.Dir, 0o755)

	l := &Logger{
		cfg:       cfg,
		entries:   make(map[string]*Entry),
		dayBytes:  make(map[string]int64),
		budgetHit: make(map[string]bool),
		summary:   &dailyWriter{dir: cfg.Dir, prefix: "requests"},
		errors:    &dailyWriter{dir: cfg.Dir, prefix: "errors"},
		retries:   newRetryTracker(4096, 30*time.Minute),
	}
	if cfg.Index {
		idx, err := openIndex(filepath.Join(cfg.Dir, "index.sqlite"))
		if err != nil {
			// A broken index must not stop request logging.
			slog.Error("requestlog: log index unavailable, files only", "error", err)
		} else {
			l.idx = idx
		}
	}
	return l
}

// Open creates (or reuses) a body log entry for the given request.
// Files are placed in logs/YYYY-MM-DD/HH-MM-SS.sss_{request_id}.jsonl.
//
// The entry stops being written once the day's body budget is spent: the summary
// line is unaffected, so the accounting of who spent what survives even when the
// bodies are switched off for the rest of the day.
func (l *Logger) Open(reqID string, startTime time.Time) (*Entry, error) {
	if !l.cfg.Bodies {
		return nil, fmt.Errorf("body logging is disabled")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if e, ok := l.entries[reqID]; ok {
		return e, nil
	}

	dateDir := filepath.Join(l.cfg.Dir, startTime.Format("2006-01-02"))
	day := startTime.Format("2006-01-02")
	if l.overBudgetLocked(day, dateDir) {
		return nil, fmt.Errorf("body log budget for %s is spent", day)
	}
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		return nil, err
	}

	filename := fmt.Sprintf("%s_%s.jsonl", startTime.Format("15-04-05.000"), reqID)
	path := filepath.Join(dateDir, filename)

	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	e := &Entry{file: f, path: path, account: func(n int) { l.account(day, int64(n)) }}
	l.entries[reqID] = e
	return e, nil
}

// overBudgetLocked reports whether the day's bodies have reached the budget,
// seeding the running total from disk the first time the day is seen so a restart
// does not hand the day a fresh allowance.
func (l *Logger) overBudgetLocked(day, dir string) bool {
	if l.cfg.BodyBudget <= 0 {
		return false
	}
	if _, seen := l.dayBytes[day]; !seen {
		l.dayBytes[day] = dirSize(dir)
	}
	if l.budgetHit[day] {
		return true
	}
	if l.dayBytes[day] >= l.cfg.BodyBudget {
		l.budgetHit[day] = true
		slog.Warn("requestlog: body log budget reached, bodies off until midnight",
			"day", day, "bytes", l.dayBytes[day], "budget", l.cfg.BodyBudget)
		return true
	}
	return false
}

// account adds written bytes to the day's running total.
func (l *Logger) account(day string, n int64) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dayBytes[day] += n
}

// dirSize is the total size of the regular files directly inside dir.
func dirSize(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// GetEntry returns the entry for a request ID, or nil if not found.
func (l *Logger) GetEntry(reqID string) *Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entries[reqID]
}

// CloseEntry closes the log file for a request and removes it from the map.
func (l *Logger) CloseEntry(reqID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[reqID]; ok {
		e.file.Close()
		delete(l.entries, reqID)
	}
}

// BodiesEnabled reports whether the per-request body files are being written.
func (l *Logger) BodiesEnabled() bool {
	return l != nil && l.cfg.Bodies
}

// Config returns the active configuration.
func (l *Logger) Config() Config {
	if l == nil {
		return Config{}
	}
	return l.cfg
}

// Classify fills the probe and retry fields and reports whether the record
// should be written at all.
func (l *Logger) Classify(rec *Record) bool {
	if l == nil {
		return false
	}
	rec.Probe = IsProbe(rec.Method, rec.Path)
	if rec.BodyHash != "" {
		attempt, firstID := l.retries.Observe(rec.BodyHash, rec.RequestID, time.Now())
		rec.Attempt = attempt
		rec.RetryOf = firstID
	}
	if rec.Probe && !l.cfg.LogProbes {
		return false
	}
	return true
}

// Emit writes the summary line, the error line when the request failed, and the
// index row. Call once per request, after the handler returned.
func (l *Logger) Emit(rec Record) {
	if l == nil {
		return
	}
	if rec.TS.IsZero() {
		rec.TS = time.Now().UTC()
	}
	if !l.Classify(&rec) {
		return
	}
	rec.TS = rec.TS.UTC()
	if rec.LatencyMS < 0 {
		rec.LatencyMS = 0
	}

	line, err := json.Marshal(struct {
		Dir string `json:"dir"`
		Record
	}{Dir: "summary", Record: rec})
	if err != nil {
		slog.Error("requestlog: marshal summary failed", "error", err, "request_id", rec.RequestID)
		return
	}
	day := rec.TS.Format("2006-01-02")
	if err := l.summary.writeLine(day, line); err != nil {
		slog.Error("requestlog: write summary failed", "error", err, "request_id", rec.RequestID)
	}

	if rec.Status >= 400 || rec.ErrorCode != "" {
		if err := l.errors.writeLine(day, line); err != nil {
			slog.Error("requestlog: write error line failed", "error", err, "request_id", rec.RequestID)
		}
	}

	if l.idx != nil {
		if err := l.idx.insert(rec); err != nil {
			slog.Error("requestlog: index insert failed", "error", err, "request_id", rec.RequestID)
		}
	}
}

// Stats answers one aggregation from the index.
func (l *Logger) Stats(q StatsQuery) (StatsResult, error) {
	if l == nil || l.idx == nil {
		return StatsResult{}, fmt.Errorf("log index is disabled (enable log_index)")
	}
	return l.idx.query(q)
}

// Close flushes and closes every sink.
func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.summary.close()
	l.errors.close()
	if l.idx != nil {
		if err := l.idx.close(); err != nil {
			slog.Error("requestlog: closing index failed", "error", err)
		}
	}
}

// dailyWriter appends lines to <dir>/<prefix>-YYYY-MM-DD.jsonl, rotating by day.
type dailyWriter struct {
	dir    string
	prefix string

	mu   sync.Mutex
	day  string
	file *os.File
}

func (w *dailyWriter) writeLine(day string, line []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil || w.day != day {
		if w.file != nil {
			_ = w.file.Close()
			w.file = nil
		}
		path := filepath.Join(w.dir, fmt.Sprintf("%s-%s.jsonl", w.prefix, day))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		w.file, w.day = file, day
	}
	_, err := w.file.Write(append(line, '\n'))
	return err
}

func (w *dailyWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
}

// ── Global singleton ──

var globalLogger *Logger

// Init initialises the global logger. Called once at startup.
func Init(cfg Config) {
	globalLogger = NewLogger(cfg)
}

// Get returns the global logger, or nil when logging is off.
func Get() *Logger {
	return globalLogger
}

// Enabled reports whether summary logging is active.
func Enabled() bool {
	return globalLogger != nil
}

// SortedDays returns day directory names in ascending order.
func SortedDays(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	days := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if _, err := time.Parse("2006-01-02", name); err != nil {
			continue
		}
		days = append(days, name)
	}
	sort.Strings(days)
	return days
}
