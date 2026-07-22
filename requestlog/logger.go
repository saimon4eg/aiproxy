package requestlog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// Entry is a single request log file (one JSONL file per request).
type Entry struct {
	mu   sync.Mutex
	file *os.File
	path string
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
	if _, err := e.file.Write(line); err != nil {
		slog.Error("requestlog: write failed", "error", err, "path", e.path)
	}
}

// Logger manages log entries, keyed by request ID. Thread-safe.
type Logger struct {
	dir     string
	mu      sync.Mutex
	entries map[string]*Entry
}

// NewLogger creates a new Logger. Creates the output directory if needed.
func NewLogger(dir string) *Logger {
	os.MkdirAll(dir, 0755)
	return &Logger{dir: dir, entries: make(map[string]*Entry)}
}

// Open creates (or reuses) a log entry for the given request.
// Files are placed in logs/YYYY-MM-DD/HH-MM-SS.sss_{request_id}.jsonl.
func (l *Logger) Open(reqID string, startTime time.Time) (*Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if e, ok := l.entries[reqID]; ok {
		return e, nil
	}

	dateDir := filepath.Join(l.dir, startTime.Format("2006-01-02"))
	if err := os.MkdirAll(dateDir, 0755); err != nil {
		return nil, err
	}

	filename := fmt.Sprintf("%s_%s.jsonl", startTime.Format("15-04-05.000"), reqID)
	path := filepath.Join(dateDir, filename)

	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	e := &Entry{file: f, path: path}
	l.entries[reqID] = e
	return e, nil
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

// ── Global singleton ──

var globalLogger *Logger

// Init initialises the global logger. Called once at startup when debug is on.
func Init(dir string) {
	globalLogger = NewLogger(dir)
}

// Get returns the global logger, or nil if debug is off / not initialised.
func Get() *Logger {
	return globalLogger
}
