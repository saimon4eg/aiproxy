package requestlog

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// probePaths are endpoints clients poll to check that the proxy is alive or to
// enumerate models. They never generate anything.
//
// They matter beyond disk space: on a real day 311 of 1938 requests were probes,
// and they produced 176 404s and 86 401s — 13% of all log records were phantom
// errors that made "how many 403s yesterday" wrong.
var probePaths = map[string]bool{
	"/v1/models":     true,
	"/models":        true,
	"/props":         true,
	"/v1/props":      true,
	"/api/tags":      true,
	"/v1/api/tags":   true,
	"/version":       true,
	"/api/v1/models": true,
	"/v1beta/models": true,
	"/health":        true,
	"/usage":         true,
	"/admin/stats":   true,
	"/admin/usage":   true,
	"/":              true,
}

// IsProbe reports whether a request only checks for liveness or a model list.
func IsProbe(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	clean := path
	if i := strings.IndexByte(clean, '?'); i >= 0 {
		clean = clean[:i]
	}
	if probePaths[clean] {
		return true
	}
	// /v1beta/models/{model} is a metadata lookup, not a generation.
	return strings.HasPrefix(clean, "/v1beta/models/") || strings.HasPrefix(clean, "/api/v1/models/")
}

// retryTracker recognises a client repeating the same request body.
//
// This is the visible trace of a retry loop — what the logs showed was 1627
// messages with only 80 distinct bodies, one of them sent 268 times in a day,
// and another repeated 115 times while it was returning 502. No client sends an
// attempt counter, and the inbound message id is not available either, but the
// body hash plus a time window identifies the loop without any client change.
type retryTracker struct {
	mu      sync.Mutex
	entries map[string]*retryEntry
	max     int
	ttl     time.Duration
}

type retryEntry struct {
	firstID string
	count   int
	last    time.Time
}

func newRetryTracker(max int, ttl time.Duration) *retryTracker {
	if max <= 0 {
		max = 4096
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &retryTracker{entries: make(map[string]*retryEntry), max: max, ttl: ttl}
}

// Observe records one occurrence and returns the attempt number and the request
// id of the first occurrence. attempt == 1 means "first time seen".
func (t *retryTracker) Observe(hash, requestID string, now time.Time) (attempt int, firstID string) {
	if t == nil || hash == "" {
		return 0, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.entries) >= t.max {
		t.evictLocked(now)
	}
	entry, ok := t.entries[hash]
	if !ok || now.Sub(entry.last) > t.ttl {
		t.entries[hash] = &retryEntry{firstID: requestID, count: 1, last: now}
		return 1, ""
	}
	entry.count++
	entry.last = now
	if entry.count <= 1 {
		return entry.count, ""
	}
	return entry.count, entry.firstID
}

// evictLocked drops expired entries first, then the oldest one, so a busy proxy
// cannot grow this map without bound.
func (t *retryTracker) evictLocked(now time.Time) {
	for hash, entry := range t.entries {
		if now.Sub(entry.last) > t.ttl {
			delete(t.entries, hash)
		}
	}
	if len(t.entries) < t.max {
		return
	}
	var oldestHash string
	var oldest time.Time
	for hash, entry := range t.entries {
		if oldestHash == "" || entry.last.Before(oldest) {
			oldestHash, oldest = hash, entry.last
		}
	}
	if oldestHash != "" {
		delete(t.entries, oldestHash)
	}
}
