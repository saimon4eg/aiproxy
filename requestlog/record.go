package requestlog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Record is one request's summary line.
//
// It exists because the four body lines (user→proxy, proxy→upstream,
// upstream→proxy, proxy→user) each hold a different slice of the truth: the
// status is only on the upstream line, the model only inside a request body, the
// upstream URL only on the outgoing line. Answering "who spent what" from that
// means reassembling every file. One summary line per request answers it directly.
type Record struct {
	RequestID string    `json:"request_id"`
	TS        time.Time `json:"ts"`

	Method string `json:"method"`
	Path   string `json:"path"`

	Model         string `json:"model,omitempty"`
	ModelResolved string `json:"model_resolved,omitempty"`
	Provider      string `json:"provider,omitempty"`

	UpstreamURL    string `json:"upstream_url,omitempty"`
	Status         int    `json:"status"`
	ErrorCode      string `json:"error_code,omitempty"`
	Error          string `json:"error,omitempty"`
	UpstreamCalled bool   `json:"upstream_called"`
	Reason         string `json:"reason,omitempty"`

	LatencyMS int64 `json:"latency_ms"`
	TTFBMS    int64 `json:"ttfb_ms,omitempty"`
	Stream    bool  `json:"stream"`

	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`

	Client    string `json:"client"`
	Profile   string `json:"profile"`
	SessionID string `json:"session_id"`
	Task      string `json:"task"`
	Origin    string `json:"origin"`

	// Attempt/RetryOf come from the body hash: the same request repeated is the
	// only visible trace of a client retry loop, and no client sends it.
	Attempt  int    `json:"attempt,omitempty"`
	RetryOf  string `json:"retry_of,omitempty"`
	BodyHash string `json:"body_hash,omitempty"`

	// RateLimit carries the upstream's own quota headers. They are the only
	// signal of how close a subscription is to its window limit.
	RateLimit map[string]string `json:"ratelimit,omitempty"`

	Probe    bool  `json:"probe,omitempty"`
	BytesOut int64 `json:"bytes_out,omitempty"`
}

// Recorder accumulates one request's record across middleware, handlers and the
// logging transport, which all run on the same goroutine but at different depths.
type Recorder struct {
	mu  sync.Mutex
	rec Record
}

// NewRecorder starts a record at the moment the request entered the process.
func NewRecorder(requestID string, start time.Time) *Recorder {
	return &Recorder{rec: Record{
		RequestID: requestID,
		TS:        start.UTC(),
		LatencyMS: -1,
	}}
}

// Mutate applies fn to the record under a lock. Every writer goes through this
// so a handler and the logging transport can touch different fields without a
// race.
func (r *Recorder) Mutate(fn func(*Record)) {
	if r == nil || fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.rec)
}

// Snapshot returns a copy for serialisation.
func (r *Recorder) Snapshot() Record {
	if r == nil {
		return Record{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.rec
	if out.RateLimit != nil {
		limits := make(map[string]string, len(out.RateLimit))
		for k, v := range out.RateLimit {
			limits[k] = v
		}
		out.RateLimit = limits
	}
	return out
}

type recorderKey struct{}

// ContextWithRecorder attaches the record to the request context.
func ContextWithRecorder(ctx context.Context, r *Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

// RecorderFromContext returns the record, or nil when logging is disabled.
func RecorderFromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(recorderKey{}).(*Recorder)
	return r
}

// Mutate is the one call sites need: record a fact without plumbing checks.
func Mutate(ctx context.Context, fn func(*Record)) {
	RecorderFromContext(ctx).Mutate(fn)
}

// ClientName identifies the calling program.
//
// A header the client sets about itself wins over the User-Agent, because a
// single User-Agent covers a chat turn plus approval, compression, title
// generation and background review.
func ClientName(h http.Header) string {
	if v := strings.TrimSpace(h.Get("X-Hermes-Client")); v != "" {
		return v
	}
	ua := strings.TrimSpace(h.Get("User-Agent"))
	if ua == "" {
		return ""
	}
	if i := strings.IndexByte(ua, ' '); i > 0 {
		// "claude-cli/2.1.311 (external, sdk-cli)" → "claude-cli/2.1.311"
		return strings.TrimSuffix(strings.TrimSpace(ua[:i]), "/")
	}
	return ua
}

// BodyHash fingerprints a request body so repeats can be recognised.
func BodyHash(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
}
