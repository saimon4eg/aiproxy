package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/whtsky/copilot2api/requestlog"
)

// debugLogMiddleware logs request headers (sanitised) and body (truncated)
// for debugging. Only active when log level is DEBUG.
// When requestlog is enabled, also writes a user→proxy JSONL entry to disk.
func debugLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip health check — Docker polls every 30s, floods logs.
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		reqID := requestlog.RequestIDFromContext(r.Context())
		startTime := time.Now()

		// ── Open log file (no-op when debug is off) ──
		var entry *requestlog.Entry
		if log := requestlog.Get(); log != nil && reqID != "" {
			e, err := log.Open(reqID, startTime)
			if err != nil {
				slog.Error("requestlog: failed to open log entry", "req_id", reqID, "error", err)
			} else {
				entry = e
			}
		}

		// Log headers (exclude Authorization).
		hdrs := make(map[string]string, 8)
		for _, k := range []string{
			"Content-Type", "User-Agent", "Accept",
			"Copilot-Integration-Id", "X-Github-Api-Version",
			"Editor-Version", "Editor-Plugin-Version",
			"X-Request-ID", "X-Initiator", "Openai-Intent",
		} {
			if v := r.Header.Get(k); v != "" {
				hdrs[k] = v
			}
		}
		slog.Debug("request", "method", r.Method, "path", r.URL.Path, "headers", hdrs)

		// Read and log body (first 8KB), then re-wrap for downstream.
		var bodyBytes []byte
		if r.Body != nil && r.Method != "GET" && r.Method != "HEAD" {
			body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
			if err == nil {
				limit := len(body)
				if limit > 8192 {
					limit = 8192
				}
				slog.Debug("request body", "path", r.URL.Path, "body", string(body[:limit]))
				r.Body = io.NopCloser(bytes.NewReader(body))
				bodyBytes = body
			}
		}

		// ── File log: user→proxy ──
		if entry != nil {
			entry.Log(map[string]interface{}{
				"dir":     "user→proxy",
				"method":  r.Method,
				"path":    r.URL.RequestURI(),
				"headers": hdrs,
				"body":    truncStr(bodyBytes, 8<<10),
			})
		}

		next.ServeHTTP(w, r)
	})
}

func truncStr(b []byte, limit int) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + "\n[... truncated ...]"
}
