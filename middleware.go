package main

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/whtsky/copilot2api/providers"
	"github.com/whtsky/copilot2api/requestlog"
)

// requestIDMiddleware assigns a request ID and owns the request record.
//
// It is the outermost middleware, which matters: anything that answers on its
// own (CORS preflight, a rate-limit 429) must be inside this wrapper, or the
// rejection would never be seen, recorded, or counted as an error.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			b := make([]byte, 8)
			rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)

		startTime := time.Now()
		ctx := providers.ContextWithLogger(r.Context(), slog.With("request_id", id))
		ctx = requestlog.ContextWithRequestID(ctx, id)

		var rec *requestlog.Recorder
		if requestlog.Enabled() {
			rec = requestlog.NewRecorder(id, startTime)
			ctx = requestlog.ContextWithRecorder(ctx, rec)
			rec.Mutate(func(record *requestlog.Record) {
				record.Method = r.Method
				record.Path = r.URL.RequestURI()
				record.Client = requestlog.ClientName(r.Header)
				// Attribution headers are optional: the proxy records them when a
				// client sends them and writes nothing when it does not.
				record.Profile = r.Header.Get("X-Hermes-Profile")
				record.SessionID = r.Header.Get("X-Hermes-Session-Id")
				record.Task = r.Header.Get("X-Hermes-Task")
				record.Origin = r.Header.Get("X-Hermes-Origin")
			})
		}

		r = r.WithContext(ctx)

		writer := requestlog.NewLoggedResponseWriter(w, id, rec, startTime)
		next.ServeHTTP(writer, r)
		writer.Close()

		if rec != nil {
			rec.Mutate(func(record *requestlog.Record) {
				record.LatencyMS = time.Since(startTime).Milliseconds()
			})
			requestlog.Get().Emit(rec.Snapshot())
		}
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
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// latencyMiddleware logs a one-line summary for every request.
//
// The response capture itself lives in requestIDMiddleware, outside the auth
// middleware, so rejections are seen too; this only reports.
func latencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if rec := requestlog.RecorderFromContext(r.Context()); rec != nil {
			snapshot := rec.Snapshot()
			slog.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", snapshot.Status,
				"latency_ms", time.Since(start).Milliseconds(),
				"upstream_called", snapshot.UpstreamCalled,
			)
		}
	})
}
