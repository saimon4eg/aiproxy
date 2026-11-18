package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/whtsky/copilot2api/requestlog"
)

// ── small env helpers ──

func envString(name, flagValue, fallback string) string {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func envBool(name string) (value, ok bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return false, false
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, false
	}
	return parsed, true
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

// parseSince accepts a duration ("24h", "7d") or a date ("2026-11-12").
func parseSince(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return now.AddDate(0, 0, -7), nil
	}
	if strings.HasSuffix(raw, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid window %q", raw)
		}
		return now.AddDate(0, 0, -days), nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return now.Add(-d), nil
	}
	if day, err := time.ParseInLocation("2006-01-02", raw, time.Local); err == nil {
		return day, nil
	}
	return time.Time{}, fmt.Errorf("invalid window %q (use 24h, 7d or 2026-11-12)", raw)
}

// runStats prints a usage summary and returns a process exit code.
func runStats(dir, since, groupBy, by string, limit int) int {
	logger := requestlog.NewLogger(requestlog.Config{Dir: dir, Index: true})
	defer logger.Close()

	until := time.Now()
	from, err := parseSince(since, until)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stats:", err)
		return 2
	}
	if !requestlog.ValidateGroup(groupBy) {
		fmt.Fprintf(os.Stderr, "stats: unsupported group-by %q\n", groupBy)
		return 2
	}

	result, err := logger.Stats(requestlog.StatsQuery{
		Since: from, Until: until, GroupBy: groupBy, By: by, Limit: limit,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "stats:", err)
		return 1
	}

	fmt.Printf("window %s → %s", from.Format("2006-01-02 15:04"), until.Format("2006-01-02 15:04"))
	if groupBy != "" {
		fmt.Printf(", grouped by %s", groupBy)
	}
	if by != "" {
		fmt.Printf(" per %s", by)
	}
	fmt.Println()
	fmt.Printf("%-40s %8s %7s %12s %12s %12s\n", "KEY", "REQUESTS", "ERRORS", "INPUT_TOK", "OUTPUT_TOK", "CACHE_READ")
	for _, row := range result.Rows {
		fmt.Printf("%-40s %8d %7d %12d %12d %12d\n",
			truncateKey(row.Key), row.Requests, row.Errors,
			row.InputTokens, row.OutputTokens, row.CacheReadTokens)
	}
	fmt.Printf("%-40s %8d %7d %12d %12d %12d\n", "TOTAL",
		result.Total.Requests, result.Total.Errors,
		result.Total.InputTokens, result.Total.OutputTokens, result.Total.CacheReadTokens)
	return 0
}

func truncateKey(key string) string {
	if key == "" {
		return "-"
	}
	if len(key) > 40 {
		return key[:37] + "..."
	}
	return key
}

// adminStatsHandler serves GET /admin/stats?since=7d&group_by=client&by=day.
func adminStatsHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query := r.URL.Query()
		until := time.Now()
		from, err := parseSince(query.Get("since"), until)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		groupBy := query.Get("group_by")
		if !requestlog.ValidateGroup(groupBy) {
			http.Error(w, "unsupported group_by", http.StatusBadRequest)
			return
		}
		by := query.Get("by")
		if by != "" && by != "hour" && by != "day" {
			http.Error(w, "by must be hour or day", http.StatusBadRequest)
			return
		}
		limit, _ := strconv.Atoi(query.Get("limit"))

		logger := requestlog.NewLogger(requestlog.Config{Dir: dir, Index: true})
		defer logger.Close()
		result, err := logger.Stats(requestlog.StatsQuery{
			Since: from, Until: until, GroupBy: groupBy, By: by, Limit: limit,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

// adminUsageHandler serves GET /admin/usage: token totals per client and per model.
//
// The old /usage endpoint asks Copilot for its quota and fails on every
// deployment that has no Copilot provider; this one answers from the proxy's own
// records, so it works everywhere.
func adminUsageHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		until := time.Now()
		from, err := parseSince(r.URL.Query().Get("since"), until)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		logger := requestlog.NewLogger(requestlog.Config{Dir: dir, Index: true})
		defer logger.Close()

		byClient, err := logger.Stats(requestlog.StatsQuery{
			Since: from, Until: until, GroupBy: "client", Limit: 100,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		byModel, err := logger.Stats(requestlog.StatsQuery{
			Since: from, Until: until, GroupBy: "model", Limit: 100,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"since":     from,
			"until":     until,
			"by_client": byClient.Rows,
			"by_model":  byModel.Rows,
			"total":     byModel.Total,
		})
	}
}

// runBackfill indexes existing body logs and returns a process exit code.
func runBackfill(dir, since string) int {
	logger := requestlog.NewLogger(requestlog.Config{Dir: dir, Index: true})
	defer logger.Close()

	until := time.Now()
	from, err := parseSince(since, until)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backfill:", err)
		return 2
	}
	report, err := logger.Backfill(from, until)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backfill:", err)
		return 1
	}
	fmt.Printf("backfilled %d requests from %d files across %d days (skipped %d, probes excluded by default)\n",
		report.Records, report.Files, report.Days, report.Skipped)
	return 0
}
