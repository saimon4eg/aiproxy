package requestlog

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: the image builds with CGO_ENABLED=0
)

// index keeps every summary line queryable without reading log files.
//
// The JSONL summary stays the source of truth (it survives a corrupt index and
// is readable with grep). The index exists only so "what did background_review
// cost this week" is a query instead of a scan over 1900 files per day.
type index struct {
	mu sync.Mutex
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS requests (
	request_id      TEXT PRIMARY KEY,
	ts              INTEGER NOT NULL,
	day             TEXT NOT NULL,
	method          TEXT,
	path            TEXT,
	model           TEXT,
	model_resolved  TEXT,
	provider        TEXT,
	status          INTEGER,
	error_code      TEXT,
	reason          TEXT,
	upstream_called INTEGER,
	latency_ms      INTEGER,
	ttfb_ms         INTEGER,
	stream          INTEGER,
	input_tokens    INTEGER,
	output_tokens   INTEGER,
	cache_read_tokens  INTEGER,
	cache_write_tokens INTEGER,
	client          TEXT,
	profile         TEXT,
	session_id      TEXT,
	task            TEXT,
	origin          TEXT,
	attempt         INTEGER,
	retry_of        TEXT,
	body_hash       TEXT,
	probe           INTEGER,
	bytes_out       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_requests_ts       ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_client   ON requests(client, ts);
CREATE INDEX IF NOT EXISTS idx_requests_model    ON requests(model, ts);
CREATE INDEX IF NOT EXISTS idx_requests_task     ON requests(task, ts);
CREATE INDEX IF NOT EXISTS idx_requests_status   ON requests(status, ts);
CREATE INDEX IF NOT EXISTS idx_requests_provider ON requests(provider, ts);
`

func openIndex(path string) (*index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open log index: %w", err)
	}
	// A single writer keeps SQLite happy and is plenty for one proxy process.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create log index schema: %w", err)
	}
	return &index{db: db}, nil
}

func (i *index) close() error {
	if i == nil || i.db == nil {
		return nil
	}
	return i.db.Close()
}

func (i *index) insert(rec Record) error {
	if i == nil || i.db == nil {
		return nil
	}
	day := rec.TS.UTC().Format("2006-01-02")
	i.mu.Lock()
	defer i.mu.Unlock()
	_, err := i.db.Exec(`
INSERT OR REPLACE INTO requests (
	request_id, ts, day, method, path, model, model_resolved, provider, status,
	error_code, reason, upstream_called, latency_ms, ttfb_ms, stream,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	client, profile, session_id, task, origin, attempt, retry_of, body_hash,
	probe, bytes_out
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.RequestID, rec.TS.UTC().UnixNano(), day, rec.Method, rec.Path,
		rec.Model, rec.ModelResolved, rec.Provider, rec.Status,
		rec.ErrorCode, rec.Reason, boolInt(rec.UpstreamCalled), rec.LatencyMS,
		rec.TTFBMS, boolInt(rec.Stream),
		rec.InputTokens, rec.OutputTokens, rec.CacheReadTokens, rec.CacheWriteTokens,
		rec.Client, rec.Profile, rec.SessionID, rec.Task, rec.Origin,
		rec.Attempt, rec.RetryOf, rec.BodyHash, boolInt(rec.Probe), rec.BytesOut,
	)
	if err != nil {
		return fmt.Errorf("index request %s: %w", rec.RequestID, err)
	}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// StatsQuery describes one aggregation.
type StatsQuery struct {
	Since   time.Time
	Until   time.Time
	GroupBy string // "", client, model, provider, task, profile, origin, status, error_code
	By      string // "", hour, day
	Limit   int
}

// StatsRow is one group's totals.
type StatsRow struct {
	Key              string `json:"key"`
	Requests         int64  `json:"requests"`
	Errors           int64  `json:"errors"`
	UpstreamCalls    int64  `json:"upstream_calls"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	LatencyMS        int64  `json:"latency_ms_avg"`
}

// StatsResult is the whole answer.
type StatsResult struct {
	Since time.Time  `json:"since"`
	Until time.Time  `json:"until"`
	Rows  []StatsRow `json:"rows"`
	Total StatsRow   `json:"total"`
}

// allowedGroups is a whitelist: group names end up in SQL, so they must never
// come straight from an HTTP query parameter.
var allowedGroups = map[string]string{
	"":               "''",
	"client":         "client",
	"model":          "model",
	"model_resolved": "model_resolved",
	"provider":       "provider",
	"task":           "task",
	"profile":        "profile",
	"origin":         "origin",
	"status":         "CAST(status AS TEXT)",
	"error_code":     "error_code",
	"reason":         "reason",
	"path":           "path",
}

// ValidateGroup reports whether a grouping is supported.
func ValidateGroup(name string) bool {
	_, ok := allowedGroups[strings.ToLower(name)]
	return ok
}

func (i *index) query(q StatsQuery) (StatsResult, error) {
	result := StatsResult{Since: q.Since, Until: q.Until, Rows: []StatsRow{}}
	if i == nil || i.db == nil {
		return result, fmt.Errorf("log index is disabled")
	}

	group, ok := allowedGroups[strings.ToLower(q.GroupBy)]
	if !ok {
		return result, fmt.Errorf("unsupported group_by %q", q.GroupBy)
	}
	keyExpr := group
	switch strings.ToLower(q.By) {
	case "":
	case "hour":
		keyExpr = fmt.Sprintf("strftime('%%Y-%%m-%%d %%H:00', ts/1000000000, 'unixepoch', 'localtime') || '|' || %s", group)
	case "day":
		keyExpr = fmt.Sprintf("day || '|' || %s", group)
	default:
		return result, fmt.Errorf("unsupported by %q (hour or day)", q.By)
	}

	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	query := fmt.Sprintf(`
SELECT %s AS grp,
       COUNT(*) AS requests,
       COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0) AS errors,
       COALESCE(SUM(upstream_called),0) AS upstream_calls,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0),
       CAST(COALESCE(AVG(latency_ms),0) AS INTEGER)
FROM requests
WHERE ts >= ? AND ts < ?
GROUP BY grp
ORDER BY requests DESC
LIMIT %d`, keyExpr, limit)

	rows, err := i.db.Query(query, q.Since.UTC().UnixNano(), q.Until.UTC().UnixNano())
	if err != nil {
		return result, fmt.Errorf("query log index: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row StatsRow
		if err := rows.Scan(&row.Key, &row.Requests, &row.Errors, &row.UpstreamCalls,
			&row.InputTokens, &row.OutputTokens, &row.CacheReadTokens,
			&row.CacheWriteTokens, &row.LatencyMS); err != nil {
			return result, err
		}
		if row.Key == "|" || row.Key == "" {
			row.Key = "-"
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}

	for _, row := range result.Rows {
		result.Total.Requests += row.Requests
		result.Total.Errors += row.Errors
		result.Total.UpstreamCalls += row.UpstreamCalls
		result.Total.InputTokens += row.InputTokens
		result.Total.OutputTokens += row.OutputTokens
		result.Total.CacheReadTokens += row.CacheReadTokens
		result.Total.CacheWriteTokens += row.CacheWriteTokens
	}
	result.Total.Key = "total"
	return result, nil
}

// prune removes index rows older than the cutoff, so the index follows the same
// lifetime as the files it summarises.
func (i *index) prune(before time.Time) (int64, error) {
	if i == nil || i.db == nil {
		return 0, nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	res, err := i.db.Exec(`DELETE FROM requests WHERE ts < ?`, before.UTC().UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
