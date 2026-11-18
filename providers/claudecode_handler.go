package providers

// The request lifecycle for a claude-cli provider.
//
// One client request → one CLI process → exactly one upstream generation. The
// agent-level guarantees (tool execution, approvals, compaction, budgets) stay
// with the host; this handler only carries a model turn there and back.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/whtsky/copilot2api/requestlog"
)

// claudeStatusError carries the HTTP status the host should see, so its retry and
// fallback policy can act on the real cause instead of an opaque failure.
type claudeStatusError struct {
	Status  int
	Message string
	// Code names the cause in machine terms. It is what a client is told when the
	// status can no longer be sent; the prose stays in the log.
	Code string
}

func (e claudeStatusError) Error() string { return e.Message }

// native error codes → HTTP status, mirroring what the upstream reported.
var nativeErrorStatus = map[string]int{
	"rate_limit":            http.StatusTooManyRequests,
	"billing_error":         http.StatusPaymentRequired,
	"authentication_failed": http.StatusUnauthorized,
	"overloaded":            529,
	"server_error":          http.StatusServiceUnavailable,
}

const defaultReadIdle = 180 * time.Second

// makeClaudeCLIHandler returns the handler for type=claude-cli providers.
func (p *ProviderConfig) makeClaudeCLIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpstreamBody))
		if err != nil {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "read request body: "+err.Error())
			return
		}
		var request anthropicMessagesRequest
		if err := json.Unmarshal(body, &request); err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		sseStarted := false
		if err := p.serveClaudeRequest(w, r, &request, &sseStarted); err != nil {
			status := http.StatusInternalServerError
			if typed, ok := err.(claudeStatusError); ok {
				status = typed.Status
			}
			slog.Error("claude-cli request failed",
				"provider", p.ProviderID, "status", status, "error", err)
			if sseStarted {
				// Headers are already out. Anthropic reports a mid-stream failure
				// as a terminal error event, and clients treat that as a failed
				// turn — a bare 200 with no terminal event would look like an
				// empty answer and be retried as one.
				//
				// Only the machine-readable cause travels here. The upstream's own
				// words can read like an account verdict ("... not your usage
				// limit"), and a host that matches on words turns this retryable
				// abort into a terminal billing failure. The prose is in the log
				// line above, where it costs nothing.
				writeStreamError(w, status, clientMessage(err))
				return
			}
			writeAnthropicError(w, status, err.Error())
		}
	})
}

// clientMessage is what a client is told once the status can no longer be sent.
// It names the cause in machine terms and never quotes the upstream: a quoted
// refusal can read like an account verdict, and a host that matches on words is
// then entitled to stop a turn that should have been retried.
func clientMessage(err error) string {
	kind, code, status := "claude_unavailable", "", 0
	if typed, ok := err.(claudeStatusError); ok {
		status, code = typed.Status, typed.Code
	}
	if status > 0 {
		kind = "upstream_status_" + strconv.Itoa(status)
	}
	if code != "" {
		return "claude-cli: the turn ended before completion (" + kind + ", " + code + ")"
	}
	return "claude-cli: the turn ended before completion (" + kind + ")"
}

// writeStreamError emits the terminal error frame of an SSE response.
//
// The payload is machine-readable on purpose: the HTTP status is gone by now, so
// the status and the code are the only honest signals a client can retry on.
func writeStreamError(w http.ResponseWriter, status int, message string) {
	payload, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": message,
			"status":  status,
		},
	})
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// serveClaudeRequest runs one turn.
func (p *ProviderConfig) serveClaudeRequest(w http.ResponseWriter, r *http.Request, request *anthropicMessagesRequest, sseStarted *bool) error {
	plan, err := buildClaudeRequest(request)
	if err != nil {
		return claudeStatusError{Status: http.StatusBadRequest, Message: err.Error()}
	}

	parent := envMap(os.Environ())
	configDir := p.claudeConfigDir(parent)
	if configDir == "" {
		return claudeStatusError{Status: http.StatusUnauthorized, Message: loginHint}
	}
	if _, statErr := os.Stat(configDir); statErr != nil {
		return claudeStatusError{Status: http.StatusUnauthorized, Message: loginHint}
	}

	// idle bounds a silent gap, never the whole turn: a long generation that keeps
	// emitting deltas is allowed to run as long as it needs, while a hung child is
	// still abandoned. The client's own context is the outer bound.
	idle := p.readIdle(parent)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	proc, err := prepareClaudeProc(parent, p.Command, "", plan)
	if err != nil {
		return claudeStatusError{Status: http.StatusServiceUnavailable, Message: err.Error()}
	}

	// The relay must exist before the child starts: its URL is part of the child's
	// environment.
	relay, err := newAdmission(p.BaseURL, p.httpClient(), nil)
	if err != nil {
		_ = os.RemoveAll(proc.root)
		return claudeStatusError{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	// The CLI dials the relay from its own process with its own context, so the
	// incoming request's record has to be handed over explicitly — otherwise the
	// turn reaches Anthropic but is logged as never having gone upstream, with
	// no token counts and no quota headers.
	relay.setRecorder(requestlog.RecorderFromContext(r.Context()))
	// The cache-wire correction needs the frame the client asked for: the CLI
	// echoes it back with its own per-request context attached, and the message
	// breakpoint has to stay on the part that actually recurs.
	relay.setQueried(queriedFrame(plan.Frames))
	defer relay.close()
	proc.env = buildChildEnv(parent, relay.baseURL(), configDir,
		claudeHomeDir(configDir), plan.MaxTokens, p.ProxyHost)

	stream, err := startClaude(ctx, proc)
	if err != nil {
		_ = os.RemoveAll(proc.root)
		return claudeStatusError{Status: http.StatusServiceUnavailable, Message: err.Error()}
	}
	defer stream.close()

	if err := replayHistory(stream, plan, idle); err != nil {
		return err
	}
	// End of input: the CLI reads frames until stdin closes before it answers.
	_ = stream.stdin.Close()

	result := &answer{}
	sse := (*sseWriter)(nil)
	if plan.Stream {
		sse = newSSEWriter(w)
		// message_start belongs to the opening of the stream, not to the answer:
		// it is written the moment the headers commit, which is now deferred to
		// the first real event.
		sse.preamble = []map[string]any{{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_claude_cli", "type": "message", "role": "assistant",
				"model": plan.Model, "content": []any{}, "stop_reason": nil,
				// Input tokens are only known once the turn finishes; the CLI does
				// not report them before generating. message_delta carries the real
				// counts, and every Anthropic SDK overwrites usage there.
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}}
		// This hook decides whether the 200 may be committed at all. A refusal
		// that lands before any content keeps its own status: the host retries on
		// an overloaded or rate-limited status, while a 200 carrying the same
		// words in prose is read as a terminal failure and the turn stops dead.
		sse.before = func() error {
			status, failure, known := relay.upstreamStatus()
			if !known || status < 400 {
				*sseStarted = true
				return nil
			}
			detail := "upstream status " + strconv.Itoa(status)
			if failure != "" {
				detail += " (" + failure + ")"
			}
			return claudeStatusError{Status: status, Message: "upstream refused this generation: " + detail, Code: "upstream_refused"}
		}
	}

	blockIndex := -1
	blockOpen := false
	closeBlock := func() error {
		if !blockOpen {
			return nil
		}
		blockOpen = false
		if sse == nil {
			return nil
		}
		return sse.send(map[string]any{"type": "content_block_stop", "index": blockIndex})
	}

	for {
		event, readErr := stream.next(idle)
		if readErr != nil {
			// A dead or silent child before any content must not surface as an
			// opaque 500: the upstream's own status, when the relay saw one, is
			// the cause, and a gateway failure is what everything else is.
			if status, failure, known := relay.upstreamStatus(); known && status >= 400 {
				detail := "upstream status " + strconv.Itoa(status)
				if failure != "" {
					detail += " (" + failure + ")"
				}
				return claudeStatusError{Status: status, Message: detail + ": " + readErr.Error(), Code: "upstream_failed"}
			}
			return claudeStatusError{Status: http.StatusGatewayTimeout, Message: readErr.Error(), Code: "native_stream_failed"}
		}
		if event == nil {
			break
		}
		switch kind, _ := event["type"].(string); kind {
		case "assistant":
			message, _ := event["message"].(map[string]any)
			if message == nil {
				continue
			}
			// Native reports a failed turn on the EVENT (``error``), not inside
			// the message, and still labels the result ``subtype: success`` with
			// ``is_error: true``. Both shapes must be read here or an auth
			// failure would look like a normal empty answer.
			if code, ok := event["error"].(string); ok && code != "" {
				result.nativeError = nativeErrorText(message)
				result.nativeErrorCode = code
				continue
			}
			if messageErr, ok := message["error"]; ok && messageErr != nil {
				result.nativeError = nativeErrorText(message)
				result.nativeErrorCode, _ = event["error"].(string)
				continue
			}
			result.assistantMessages = append(result.assistantMessages, message)

		case "result":
			result.results = append(result.results, event)
			// A native failure that only carries English text: keep it so the
			// client sees the real cause instead of a generic 500.
			if failed, _ := event["is_error"].(bool); failed && result.nativeError == "" {
				if text, ok := event["result"].(string); ok && strings.TrimSpace(text) != "" {
					result.nativeError = strings.TrimSpace(text)
				}
			}

		case "stream_event":
			native, _ := event["event"].(map[string]any)
			if native == nil {
				continue
			}
			switch nested, _ := native["type"].(string); nested {
			case "message_stop":
				result.sawMessageStop = true

			case "content_block_start":
				block, _ := native["content_block"].(map[string]any)
				kind := ""
				if block != nil {
					kind, _ = block["type"].(string)
				}
				if sse != nil && (kind == "text" || kind == "thinking") {
					if err := closeBlock(); err != nil {
						return err
					}
					index := intField(native, "index")
					blockIndex = index
					blockOpen = true
					if err := sse.send(map[string]any{
						"type": "content_block_start", "index": index,
						"content_block": block,
					}); err != nil {
						return err
					}
				}

			case "content_block_delta":
				delta, _ := native["delta"].(map[string]any)
				if delta == nil {
					continue
				}
				index := intField(native, "index")
				switch deltaType, _ := delta["type"].(string); deltaType {
				case "text_delta":
					text, _ := delta["text"].(string)
					payload := result.textDelta(index, text)
					if sse != nil {
						if !blockOpen || blockIndex != index {
							if err := closeBlock(); err != nil {
								return err
							}
							blockIndex, blockOpen = index, true
						}
						if err := sse.send(payload); err != nil {
							return err
						}
					}
				case "thinking_delta":
					thinking, _ := delta["thinking"].(string)
					if sse != nil {
						if !blockOpen || blockIndex != index {
							if err := closeBlock(); err != nil {
								return err
							}
							blockIndex, blockOpen = index, true
						}
						if err := sse.send(thinkingDelta(index, thinking)); err != nil {
							return err
						}
					}
				case "signature_delta":
					// The signature is what makes a thinking block replayable: without
					// it the next request drops the block and the model re-thinks from
					// scratch. Forgetting to forward this looked like "thinking works"
					// while multi-turn reasoning silently lost its transcript.
					signature, _ := delta["signature"].(string)
					if sse != nil && signature != "" {
						if !blockOpen || blockIndex != index {
							if err := closeBlock(); err != nil {
								return err
							}
							blockIndex, blockOpen = index, true
						}
						if err := sse.send(map[string]any{
							"type": "content_block_delta", "index": index,
							"delta": map[string]any{"type": "signature_delta", "signature": signature},
						}); err != nil {
							return err
						}
					}
				}
			}
		}
	}

	if err := closeBlock(); err != nil {
		return err
	}

	// The relay's capture is authoritative for usage and stop reason: native's
	// own recovery can replace what the stream said. Nothing to wait for when the
	// child never reached the upstream (a fake CLI, or a failure before the call).
	used, _ := relay.stats()
	var captured *admittedResponse
	if used {
		captured = relay.wait(idle)
	}
	if captured != nil {
		if !captured.Complete || captured.Status >= 400 {
			used, denied := relay.stats()
			detail := fmt.Sprintf("first upstream attempt: status %d", captured.Status)
			if !captured.Complete {
				detail += ", capture incomplete"
			}
			if used {
				detail += fmt.Sprintf(", subsequent attempts denied: %d", denied)
			}
			if result.nativeError != "" {
				detail += ": " + result.nativeError
			}
			// A 200 the upstream cut short has no status of its own to report, so
			// it is a gateway failure — which is a retryable class for the host,
			// unlike an answer that merely mentions a problem in prose.
			status := captured.Status
			if status < 400 {
				status = http.StatusBadGateway
			}
			return claudeStatusError{Status: status, Message: "incomplete upstream response (" + detail + ")", Code: "incomplete_upstream_response"}
		}
	}

	// A locally denied native recovery attempt, or a refusal, must not turn a
	// complete upstream answer into a failure.
	admitted := captured != nil && captured.Complete && captured.Status < 400
	if !admitted {
		if err := nativeFailure(result); err != nil {
			return err
		}
	}

	// The CLI always streams to the relay, so the transport's own stream
	// detection describes the CLI→Anthropic hop, not what this client asked for.
	// The record describes the client's request, so it is set from the plan.
	requestlog.Mutate(r.Context(), func(rec *requestlog.Record) {
		rec.Stream = plan.Stream
	})
	if len(result.results) == 0 || len(result.assistantMessages) == 0 || !result.sawMessageStop {
		return fmt.Errorf("incomplete native response: assistant, message_stop and one result are required")
	}

	// Text that never arrived as deltas (non-streaming path, or a wrapper that
	// suppressed partial messages) is emitted here.
	finalText := result.finalText()
	emitted := result.emittedText.String()
	if sse != nil && finalText != emitted {
		if strings.HasPrefix(finalText, emitted) {
			tail := finalText[len(emitted):]
			if tail != "" {
				if !blockOpen {
					blockIndex++
					blockOpen = true
					if err := sse.send(map[string]any{
						"type": "content_block_start", "index": blockIndex,
						"content_block": map[string]any{"type": "text", "text": ""},
					}); err != nil {
						return err
					}
				}
				payload := result.textDelta(blockIndex, tail)
				if err := sse.send(payload); err != nil {
					return err
				}
			}
			if err := closeBlock(); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("native final text differs from the incremental stream")
		}
	}

	response := result.buildResponse(plan.Model, plan)

	if sse == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return json.NewEncoder(w).Encode(response)
	}

	// Tool calls and their signed thinking blocks are announced only now: a partial
	// tool batch must never reach the host.
	for _, block := range responseBlocks(response) {
		if block["type"] != "tool_use" {
			continue
		}
		blockIndex++
		payload := map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": block}
		if err := sse.send(payload); err != nil {
			return err
		}
		if err := sse.send(map[string]any{"type": "content_block_stop", "index": blockIndex}); err != nil {
			return err
		}
	}

	usage := responseUsage(response)
	if err := sse.send(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": response["stop_reason"], "stop_sequence": nil},
		"usage": usage,
	}); err != nil {
		return err
	}
	return sse.send(map[string]any{"type": "message_stop"})
}

// replayHistory feeds prior turns, then the querying frame.
//
// A historical user frame is written with shouldQuery=false and must come back as
// a zero-turn acknowledgement; anything else means this CLI does not support the
// replay contract we depend on.
func replayHistory(stream *claudeStream, plan *claudeRequest, idle time.Duration) error {
	for index, frame := range plan.Frames {
		historical := frame.Type == "user" && index < len(plan.Frames)-1
		if historical {
			no := false
			frame.ShouldQuery = &no
		} else {
			frame.ShouldQuery = nil
		}
		if err := stream.send(frame); err != nil {
			return claudeStatusError{Status: http.StatusBadGateway, Message: err.Error()}
		}
		if !historical {
			continue
		}
		for {
			ack, err := stream.next(idle)
			if err != nil {
				return err
			}
			if ack == nil {
				return fmt.Errorf("native exited before acknowledging replayed history")
			}
			if ack["type"] != "result" {
				continue
			}
			turns := intField(ack, "num_turns")
			if failed, _ := ack["is_error"].(bool); failed || turns != 0 {
				return fmt.Errorf("native does not support history replay: expected a zero-turn acknowledgement")
			}
			break
		}
	}
	return nil
}

// nativeFailure turns a native-reported error into a status-carrying error.
func nativeFailure(a *answer) error {
	if a.nativeError == "" {
		return nil
	}
	status, ok := nativeErrorStatus[a.nativeErrorCode]
	if !ok {
		// Native said something went wrong but named no code we map: surface its
		// own words as a gateway failure rather than an empty success.
		return claudeStatusError{Status: http.StatusBadGateway, Message: a.nativeError, Code: "native_failure"}
	}
	message := a.nativeError
	if a.nativeErrorCode == "authentication_failed" {
		message = loginHint + " (native: " + a.nativeError + ")"
	}
	return claudeStatusError{Status: status, Message: message, Code: a.nativeErrorCode}
}

func nativeErrorText(message map[string]any) string {
	content, _ := message["content"].([]any)
	var parts []string
	for _, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func intField(m map[string]any, key string) int {
	if value, ok := m[key].(float64); ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
		return int(value)
	}
	return 0
}

// responseBlocks reads the response's content without assuming the shape: a
// handler panic here would drop the connection with no error at all.
func responseBlocks(response map[string]any) []map[string]any {
	blocks, _ := response["content"].([]map[string]any)
	return blocks
}

// responseUsage reads the response's usage the same way.
func responseUsage(response map[string]any) map[string]any {
	usage, _ := response["usage"].(map[string]any)
	if usage == nil {
		return map[string]any{}
	}
	return usage
}

func envMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if index := strings.IndexByte(entry, '='); index > 0 {
			out[entry[:index]] = entry[index+1:]
		}
	}
	return out
}

// readIdle is how long the CLI may stay silent before the turn is abandoned.
func (p *ProviderConfig) readIdle(env map[string]string) time.Duration {
	if p.ReadIdleSeconds > 0 {
		return time.Duration(p.ReadIdleSeconds) * time.Second
	}
	if raw := strings.TrimSpace(env["CLAUDE_CLI_TIMEOUT"]); raw != "" {
		if seconds, err := time.ParseDuration(raw); err == nil && seconds > 0 {
			return seconds
		}
	}
	return defaultReadIdle
}
