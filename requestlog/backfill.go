package requestlog

// Backfill turns the per-request body files written before this logger existed
// into index rows.
//
// Without it the 2.2 GB of history — 1900 files a day — stays unqueryable, which
// is the situation the index was built to end. Old files carry no token counts
// (the stream was truncated at 8 KB, so usage never landed), so a backfilled row
// has requests, statuses and errors but zero tokens; that asymmetry is recorded
// rather than hidden.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// BackfillReport says what one pass did.
type BackfillReport struct {
	Days    int
	Files   int
	Records int
	Skipped int
}

// Backfill indexes existing body logs between the two dates (inclusive).
func (l *Logger) Backfill(since, until time.Time) (BackfillReport, error) {
	var report BackfillReport
	if l == nil || l.idx == nil {
		return report, fmt.Errorf("log index is disabled (enable log_index)")
	}
	dir := l.cfg.Dir

	for _, day := range SortedDays(dir) {
		date, err := time.ParseInLocation("2006-01-02", day, time.Local)
		if err != nil {
			continue
		}
		if date.Before(since.Truncate(24*time.Hour)) || date.After(until) {
			continue
		}
		report.Days++
		dayPath := filepath.Join(dir, day)
		entries, err := os.ReadDir(dayPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			report.Files++
			record, ok := readLegacyEntry(filepath.Join(dayPath, entry.Name()))
			if !ok {
				report.Skipped++
				continue
			}
			if record.Probe && !l.cfg.LogProbes {
				report.Skipped++
				continue
			}
			if err := l.idx.insert(record); err != nil {
				return report, err
			}
			report.Records++
		}
	}
	return report, nil
}

// readLegacyEntry rebuilds one summary from the four body lines of one request.
func readLegacyEntry(path string) (Record, bool) {
	file, err := os.Open(path)
	if err != nil {
		return Record{}, false
	}
	defer file.Close()

	var record Record
	record.RequestID = requestIDFromFilename(path)
	var seenUser, seenUpstream bool

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row struct {
			TS      string            `json:"ts"`
			Dir     string            `json:"dir"`
			Method  string            `json:"method"`
			Path    string            `json:"path"`
			URL     string            `json:"url"`
			Status  int               `json:"status"`
			Body    string            `json:"body"`
			Headers map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			continue
		}
		switch row.Dir {
		case "user→proxy":
			seenUser = true
			record.Method = row.Method
			record.Path = row.Path
			if ts, err := time.Parse(time.RFC3339Nano, row.TS); err == nil {
				record.TS = ts.UTC()
			}
			if row.Body != "" {
				model, stream := peekLegacyBody([]byte(row.Body))
				record.Model = model
				record.Stream = stream
				record.BodyHash = BodyHash([]byte(row.Body))
			}
			header := func(name string) string {
				for key, value := range row.Headers {
					if strings.EqualFold(key, name) {
						return value
					}
				}
				return ""
			}
			if ua := header("User-Agent"); ua != "" {
				record.Client = strings.TrimSuffix(strings.TrimSpace(strings.SplitN(ua, " ", 2)[0]), "/")
			}
			record.Profile = header("X-Hermes-Profile")
			record.SessionID = header("X-Hermes-Session-Id")
			record.Task = header("X-Hermes-Task")
			record.Origin = header("X-Hermes-Origin")
		case "proxy→upstream":
			record.UpstreamCalled = true
			record.UpstreamURL = row.URL
		case "upstream→proxy":
			seenUpstream = true
			if row.Status != 0 {
				record.Status = row.Status
			}
			if limits := ratelimitHeadersFromMap(row.Headers); len(limits) > 0 {
				record.RateLimit = limits
			}
		case "proxy→user":
			if row.Status != 0 {
				record.Status = row.Status
			}
		}
	}
	if !seenUser {
		return Record{}, false
	}
	if !seenUpstream && record.Status == 0 {
		record.Reason = "no_upstream"
	}
	if record.Status >= 400 {
		record.ErrorCode = "legacy_" + strconv.Itoa(record.Status)
	}
	if record.TS.IsZero() {
		if info, err := os.Stat(path); err == nil {
			record.TS = info.ModTime().UTC()
		}
	}
	record.Probe = IsProbe(record.Method, record.Path)
	return record, true
}

// peekLegacyBody pulls the model and streaming flag from a logged request body.
//
// Legacy bodies were cut at 8 KB with a "[... truncated ...]" marker, so a full JSON
// parse fails for most of them. Do NOT assume the model survived in that head: the
// encoder sorts keys, so `model` lands after `messages`, and a request whose history
// exceeded 8 KB was logged with neither — measured on real traffic, a 25 KB body lost
// both `model` and `messages`. The fallback therefore scans for the key rather than
// counting on its position.
func peekLegacyBody(body []byte) (string, bool) {
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		return probe.Model, probe.Stream
	}
	return looseModel(body), looseStream(body)
}

// looseModel finds `"model": "<id>"` in a truncated body.
func looseModel(body []byte) string {
	key := []byte(`"model"`)
	index := bytes.Index(body, key)
	if index < 0 {
		return ""
	}
	rest := body[index+len(key):]
	colon := bytes.IndexByte(rest, ':')
	if colon < 0 {
		return ""
	}
	rest = bytes.TrimLeft(rest[colon+1:], " 	\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return string(rest[:end])
}

// looseStream reports whether the body asked for streaming.
func looseStream(body []byte) bool {
	key := []byte(`"stream":true`)
	compact := bytes.ReplaceAll(body, []byte(" "), nil)
	return bytes.Contains(compact, key)
}

// ratelimitHeadersFromMap is ratelimitHeaders for an already flattened map.
func ratelimitHeadersFromMap(h map[string]string) map[string]string {
	var out map[string]string
	for key, value := range h {
		lower := strings.ToLower(key)
		if !strings.Contains(lower, "ratelimit") && lower != "retry-after" {
			continue
		}
		if out == nil {
			out = make(map[string]string, 4)
		}
		out[lower] = value
	}
	return out
}

// requestIDFromFilename extracts the id from "HH-MM-SS.sss_<request_id>.jsonl".
func requestIDFromFilename(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.IndexByte(name, '_'); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}
