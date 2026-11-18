package providers

// Admission control for one Claude Code request.
//
// The native CLI can attempt further upstream generations on its own even with
// --max-turns 1 and HTTP retries disabled. Left alone, one client request could
// draw the subscription twice. Because aiproxy already sits between the client
// and the upstream, the relay is a route on a private loopback listener rather
// than a separate service: the first POST /v1/messages is forwarded, every later
// attempt is refused locally.
//
// The relay also captures the streamed answer (text, thinking, tool arguments,
// usage, stop reason) before native's own recovery machinery can replace it.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/whtsky/copilot2api/requestlog"
)

// stopMarker is the terminal SSE event that proves the generation finished.
const stopMarker = "message_stop"

// admittedResponse is the upstream answer the client actually receives.
type admittedResponse struct {
	Status   int
	Header   http.Header
	Body     []byte
	Complete bool
}

// admission owns the loopback listener and its single-use policy.
type admission struct {
	listener net.Listener
	server   *http.Server
	prefix   string
	// upstreamBase is the provider's base URL without a path (e.g.
	// https://api.anthropic.com). The CLI's own path is appended to it exactly
	// once: the relay used to be handed a full URL and then append the path
	// again, producing /v1/messages/v1/messages.
	upstreamBase string
	client       *http.Client
	headers      http.Header

	// rec is the incoming request's record. The CLI makes its own HTTP calls
	// from a separate goroutine, so the middleware's context never reaches this
	// hop on its own: without an explicit attachment the LoggingTransport sees
	// no record and the turn is logged with upstream_called=false and zero
	// tokens, as if Anthropic had never been contacted.
	rec *requestlog.Recorder

	// queried is the frame the client asked the model to answer, in the shape the
	// CLI echoes back. The cache-wire helpers need it to keep the message-level
	// breakpoint on content the next request replays; nil means this request has
	// nothing to correct and the body forwards untouched.
	queried []any

	mu       sync.Mutex
	used     bool
	denied   int
	captured *admittedResponse
	// upstream is the first attempt's HTTP status, set as soon as the relay has
	// it — before the body finishes. The handler reads it before committing a 200
	// so a refusal keeps its own status instead of arriving as a 200 whose stream
	// then reports a failure in text.
	upstream int
	// upstreamFailure is the transport error when the relay could not reach the
	// upstream at all.
	upstreamFailure string
	done            chan struct{}
	closed          bool
}

// maxUpstreamBody bounds what the relay will read from the CLI. Anthropic's own
// limit is smaller, so this only ever stops a runaway.
const maxUpstreamBody = 64 << 20

// newAdmission starts the relay.
//
// upstreamBase is the provider's base URL; client is the provider's HTTP client,
// so the request inherits the configured proxy and never falls back to a direct
// connection.
func newAdmission(upstreamBase string, client *http.Client, headers http.Header) (*admission, error) {
	token := make([]byte, 24)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("generate admission route: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open admission listener: %w", err)
	}
	a := &admission{
		listener:     listener,
		prefix:       "/admit/" + hex.EncodeToString(token),
		upstreamBase: strings.TrimRight(upstreamBase, "/"),
		client:       client,
		headers:      headers.Clone(),
		done:         make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(a.prefix+"/", a.handle)
	a.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = a.server.Serve(listener) }()
	return a, nil
}

// setRecorder attaches the incoming request's record to the relay.
//
// Written by the request handler before the CLI starts, read by the relay's own
// server goroutine when the CLI calls it: a mutex, not a plain field, because
// those two goroutines have no other synchronisation edge between them.
func (a *admission) setRecorder(rec *requestlog.Recorder) {
	a.mu.Lock()
	a.rec = rec
	a.mu.Unlock()
}

func (a *admission) recorder() *requestlog.Recorder {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rec
}

// setQueried attaches the frame the client asked for. Same handover as the
// recorder: written by the request handler before the CLI starts, read by the
// relay's own goroutine when the CLI calls in.
func (a *admission) setQueried(blocks []any) {
	a.mu.Lock()
	a.queried = blocks
	a.mu.Unlock()
}

func (a *admission) queriedBlocks() []any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.queried
}

// applyCacheWire corrects the CLI's request before the generation reaches
// Anthropic, exactly as the plugin's relay did: the tool-result frame is restored
// to what the client sent and the message-level breakpoint is pinned to content
// the next request replays. Nothing else is touched — every other block, message
// and the whole `system`/`tools` inventory the CLI built is forwarded unchanged.
// A body that cannot be parsed or reconciled forwards as the CLI built it, so a
// wire change costs a cache hit and never a turn.
//
// The order is load-bearing. Restoring FIRST is what lets the breakpoint land on
// the tool result: while the CLI's reminder still sits inside that result the
// frame does not match the queried one, the replayable span is cut back to the
// assistant turn, and each round then re-writes the previous round's tool output.
func (a *admission) applyCacheWire(body []byte) []byte {
	queried := a.queriedBlocks()
	if len(queried) == 0 {
		return body
	}
	a.logWire("cli→relay", body, queried)
	restored, err := restoreQueriedTurn(body, queried)
	if err != nil {
		// Restoration is hygiene, not a gate: the frame forwards as native built
		// it (the pre-restoration behaviour), which degrades to an uncached tail.
		slog.Debug("claude-cli cache wire: tool-result frame not restored", "reason", err)
	} else {
		body = restored
	}
	pinned := pinMessageBreakpoint(body, queried)
	a.logWire("relay→upstream", pinned, queried)
	return pinned
}

// logWire records the body as native built it and as the correction left it, when
// body logging is on. A cache investigation needs both: the request-side log line
// only carries what was sent, so the CLI's own arrangement is invisible otherwise.
func (a *admission) logWire(dir string, body []byte, queried []any) {
	rec := a.recorder()
	if rec == nil {
		return
	}
	var reqID string
	rec.Mutate(func(r *requestlog.Record) { reqID = r.RequestID })
	if reqID == "" {
		return
	}
	logger := requestlog.Get()
	if logger == nil || !logger.BodiesEnabled() {
		return
	}
	entry := logger.GetEntry(reqID)
	if entry == nil {
		return
	}
	entry.Log(map[string]any{
		"dir":     dir,
		"queried": len(queried),
		"body":    requestlog.ElideBody(body, requestlog.BodyLimit()),
	})
}

// baseURL is what the CLI gets as ANTHROPIC_BASE_URL.
func (a *admission) baseURL() string {
	return "http://" + a.listener.Addr().String() + a.prefix
}

// wait blocks until the first upstream attempt has been captured, or `wait` has
// passed without one.
func (a *admission) wait(wait time.Duration) *admittedResponse {
	if wait <= 0 {
		wait = defaultReadIdle
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-a.done:
	case <-timer.C:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.captured
}

func (a *admission) stats() (used bool, denied int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.used, a.denied
}

func (a *admission) close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.mu.Unlock()
	_ = a.server.Close()
	select {
	case <-a.done:
	default:
		close(a.done)
	}
}

// handle admits the first Messages request and forwards it.
//
// The admission slot is claimed under the same lock that checks it, so a
// concurrent probe can never steal a generation's slot: probes and other paths
// never touch `used` at all.
func (a *admission) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, a.prefix)
	if path == "" {
		path = "/"
	}

	if path != "/v1/messages" {
		// Health/hello probes from the CLI pass through without consuming the
		// admission slot; only a real generation counts.
		a.forward(w, r, path, false)
		return
	}

	a.mu.Lock()
	if a.used {
		a.denied++
		a.mu.Unlock()
		writeAnthropicError(w, http.StatusTooManyRequests,
			"admission: this request already used its single upstream attempt")
		return
	}
	a.used = true
	a.mu.Unlock()

	a.forward(w, r, path, true)
}

func (a *admission) forward(w http.ResponseWriter, r *http.Request, path string, capture bool) {
	target := a.upstreamBase + path
	if query := r.URL.RawQuery; query != "" {
		target += "?" + query
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpstreamBody))
	if err != nil {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, "read request body: "+err.Error())
		return
	}
	// Only a real generation gets the cache-wire correction: probes are not
	// generations, and their bodies are not Messages payloads at all.
	if capture {
		body = a.applyCacheWire(body)
	}
	// The generation's upstream hop is accounted for in the incoming request's
	// record. Probes (models, health, count_tokens) pass through untracked: they
	// are not generations, and writing them into the same record would overwrite
	// the real turn's status and tokens.
	ctx := r.Context()
	if capture {
		if rec := a.recorder(); rec != nil {
			ctx = requestlog.ContextWithRecorder(ctx, rec)
		}
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "build upstream request: "+err.Error())
		return
	}

	// Native's own headers carry the subscription identity; forward them
	// untouched. Only the provider-supplied headers fill genuine gaps.
	for key, values := range r.Header {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "content-length" || lower == "connection" ||
			lower == "accept-encoding" {
			// accept-encoding is dropped so Go negotiates and transparently
			// decompresses the answer. Forwarding the client's list made Go skip
			// its own decompression while the relay stripped Content-Encoding,
			// which handed the CLI compressed bytes labelled as plain text.
			continue
		}
		req.Header[key] = append([]string(nil), values...)
	}
	for key, values := range a.headers {
		if req.Header.Get(key) == "" {
			req.Header[key] = append([]string(nil), values...)
		}
	}

	resp, err := a.client.Do(req)
	if err != nil {
		a.recordUpstream(http.StatusBadGateway, err.Error())
		a.record(&admittedResponse{Status: http.StatusBadGateway, Header: http.Header{}, Complete: false})
		writeAnthropicError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	a.recordUpstream(resp.StatusCode, "")

	streaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	copyAdmissionHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if !streaming {
		payload, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			a.record(&admittedResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), Complete: false})
			return
		}
		_, _ = w.Write(payload)
		if capture {
			// Close before recording: the metered body publishes token counts when
			// it is closed, and a.record() is what unblocks the request handler.
			// Signalling first would let the summary be emitted with zero tokens.
			_ = resp.Body.Close()
			a.record(&admittedResponse{
				Status:   resp.StatusCode,
				Header:   resp.Header.Clone(),
				Body:     payload,
				Complete: resp.StatusCode < 400,
			})
		}
		return
	}

	flusher, _ := w.(http.Flusher)
	var collected []byte
	var complete bool
	var status = resp.StatusCode

	// message_stop may straddle two reads, so a short carry-over is kept instead
	// of re-scanning everything collected so far on every chunk.
	carry := make([]byte, 0, len(stopMarker))
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if capture {
				collected = append(collected, chunk...)
			}
			if !complete {
				window := append(append(make([]byte, 0, len(carry)+len(chunk)), carry...), chunk...)
				if bytes.Contains(window, []byte(stopMarker)) {
					complete = status < 400
				}
				if len(window) > len(stopMarker)-1 {
					carry = append(carry[:0], window[len(window)-(len(stopMarker)-1):]...)
				} else {
					carry = append(carry[:0], window...)
				}
			}
			if _, writeErr := w.Write(chunk); writeErr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}

	if capture {
		// Same reason as the non-streaming path: publish the token counts before
		// the request handler is released.
		_ = resp.Body.Close()
		a.record(&admittedResponse{
			Status:   status,
			Header:   resp.Header.Clone(),
			Body:     collected,
			Complete: complete,
		})
	}
}

// recordUpstream publishes the first attempt's status the moment the relay knows
// it. Only the first attempt counts: a later (denied) attempt never changes what
// the upstream said about this generation.
func (a *admission) recordUpstream(status int, failure string) {
	a.mu.Lock()
	if a.upstream == 0 {
		a.upstream = status
		a.upstreamFailure = failure
	}
	a.mu.Unlock()
}

// upstreamStatus reports the first attempt's HTTP status once the relay has it.
// The handler calls it before committing a 200: the upstream's own status is what
// the host's retry policy acts on, and once the headers are out it is gone.
func (a *admission) upstreamStatus() (int, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.upstream == 0 {
		return 0, "", false
	}
	return a.upstream, a.upstreamFailure, true
}

// record stores the answer and releases the waiter exactly once.
func (a *admission) record(resp *admittedResponse) {
	a.mu.Lock()
	if a.captured == nil {
		a.captured = resp
	}
	closed := a.closed
	a.mu.Unlock()
	if !closed {
		select {
		case <-a.done:
		default:
			close(a.done)
		}
	}
}

// copyAdmissionHeaders forwards the upstream answer's headers, dropping the
// hop-by-hop set and the encoding negotiation Go already resolved.
//
// Content-Encoding is deliberately NOT dropped: the request carried no
// Accept-Encoding, so Go negotiated gzip itself and decompressed the body,
// removing the header. If one is still present the body really is encoded, and
// the CLI must be told so.
func copyAdmissionHeaders(dst, src http.Header) {
	skip := map[string]bool{
		"connection": true, "keep-alive": true, "proxy-connection": true,
		"transfer-encoding": true, "upgrade": true, "trailer": true,
		"content-length": true,
	}
	keys := make([]string, 0, len(src))
	for key := range src {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if skip[strings.ToLower(key)] {
			continue
		}
		for _, value := range src[key] {
			dst.Add(key, value)
		}
	}
}

func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(fmt.Sprintf(
		`{"type":"error","error":{"type":"api_error","message":%s}}`, quoteJSON(message))))
}

func quoteJSON(s string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if r < 0x20 {
				builder.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				builder.WriteRune(r)
			}
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
