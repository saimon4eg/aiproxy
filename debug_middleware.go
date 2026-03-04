package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
)

// debugLogMiddleware logs request headers (sanitised) and body (truncated)
// for debugging. Only active when log level is DEBUG.
func debugLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		// Read and log body (first 2KB), then re-wrap for downstream.
		if r.Body != nil && r.Method != "GET" && r.Method != "HEAD" {
			body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
			if err == nil {
				limit := len(body)
				if limit > 8192 {
					limit = 8192
				}
				slog.Debug("request body", "path", r.URL.Path, "body", string(body[:limit]))
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
		}

		next.ServeHTTP(w, r)
	})
}
