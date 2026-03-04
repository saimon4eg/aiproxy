package main

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/whtsky/copilot2api/providers"
)

// requestIDMiddleware adds a unique request ID to every request.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			b := make([]byte, 8)
			rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		// Store logger with request_id in context for downstream handlers.
		ctx := providers.ContextWithLogger(r.Context(), slog.With("request_id", id))
		r = r.WithContext(ctx)
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware adds CORS headers for browser-based clients.
// Allow any origin — acceptable for localhost-only deployment.
// If exposed to a network, restrict to known origins.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, x-api-key")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// latencyMiddleware logs request latency and status.
func latencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.statusCode,
			"latency_ms", time.Since(start).Milliseconds(),
		)
	})
}

// statusRecorder captures the HTTP status code.
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher, delegating to the wrapped writer. Without it,
// the statusRecorder hides the underlying Flusher and streaming handlers (SSE)
// fail the w.(http.Flusher) assertion → "Streaming unsupported" / buffered output.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
