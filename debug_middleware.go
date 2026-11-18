package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/whtsky/copilot2api/requestlog"
)

// debugLogMiddleware logs request headers (sanitised) and body (truncated) when
// debug logging is on, and always records what the summary needs: the model, a
// fingerprint of the body, and (when body logging is enabled) the user→proxy
// entry.
func debugLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip health check — Docker polls every 30s, floods logs.
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		ctx := r.Context()
		rec := requestlog.RecorderFromContext(ctx)
		logger := requestlog.Get()

		// Log headers (exclude Authorization).
		hdrs := make(map[string]string, 16)
		for _, k := range []string{
			"Content-Type", "User-Agent", "Accept",
			"Copilot-Integration-Id", "X-Github-Api-Version",
			"Editor-Version", "Editor-Plugin-Version",
			"X-Request-ID", "X-Initiator", "Openai-Intent",
			"X-Hermes-Profile", "X-Hermes-Session-Id", "X-Hermes-Task", "X-Hermes-Origin",
		} {
			if v := r.Header.Get(k); v != "" {
				hdrs[k] = v
			}
		}
		slog.Debug("request", "method", r.Method, "path", r.URL.Path, "headers", hdrs)

		// Read the body once: the model and the retry fingerprint come from it.
		var bodyBytes []byte
		if r.Body != nil && r.Method != "GET" && r.Method != "HEAD" {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				r.Body = io.NopCloser(bytes.NewReader(body))
				bodyBytes = body
				limit := len(body)
				if limit > 8192 {
					limit = 8192
				}
				slog.Debug("request body", "path", r.URL.Path, "body", string(body[:limit]))
			}
		}

		if rec != nil && len(bodyBytes) > 0 {
			model, stream := peekRequestBody(bodyBytes)
			hash := requestlog.BodyHash(bodyBytes)
			rec.Mutate(func(record *requestlog.Record) {
				if record.Model == "" {
					record.Model = model
				}
				if stream {
					record.Stream = true
				}
				record.BodyHash = hash
			})
		}

		// ── File log: user→proxy (bodies only) ──
		if logger != nil && logger.BodiesEnabled() {
			reqID := requestlog.RequestIDFromContext(ctx)
			if entry, err := logger.Open(reqID, time.Now()); err == nil {
				entry.Log(map[string]interface{}{
					"dir":     "user→proxy",
					"method":  r.Method,
					"path":    r.URL.RequestURI(),
					"headers": hdrs,
					"body":    requestlog.ElideBody(bodyBytes, requestlog.BodyLimit()),
				})
			}
		}

		next.ServeHTTP(w, r)
	})
}

// peekRequestBody extracts the model and the streaming flag from a request body
// without holding the whole structure: both appear at the top level of every
// supported format.
func peekRequestBody(body []byte) (model string, stream bool) {
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", false
	}
	return probe.Model, probe.Stream
}
