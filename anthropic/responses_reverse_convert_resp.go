package anthropic

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// nowUnix is overridable in tests.
var nowUnix = func() int64 { return time.Now().Unix() }

// anthropicUsage is the usage block of an Anthropic message (all optional).
type anthropicUsage struct {
	input, output, cacheRead, cacheCreate int
}

func parseAnthropicUsage(v any) anthropicUsage {
	m, _ := v.(map[string]any)
	var u anthropicUsage
	u.input, _ = toInt(m["input_tokens"])
	u.output, _ = toInt(m["output_tokens"])
	u.cacheRead, _ = toInt(m["cache_read_input_tokens"])
	u.cacheCreate, _ = toInt(m["cache_creation_input_tokens"])
	return u
}

// merge overlays the fields present in an Anthropic usage object (message_delta
// repeats cumulative counts; absent fields keep the previous value).
func (u *anthropicUsage) merge(v any) {
	m, _ := v.(map[string]any)
	if m == nil {
		return
	}
	if n, ok := toInt(m["input_tokens"]); ok {
		u.input = n
	}
	if n, ok := toInt(m["output_tokens"]); ok {
		u.output = n
	}
	if n, ok := toInt(m["cache_read_input_tokens"]); ok {
		u.cacheRead = n
	}
	if n, ok := toInt(m["cache_creation_input_tokens"]); ok {
		u.cacheCreate = n
	}
}

// responsesUsage renders Responses-style usage. Anthropic reports cached tokens
// separately from input_tokens; Responses counts them inside input_tokens.
func (u anthropicUsage) responsesUsage() map[string]any {
	in := u.input + u.cacheRead + u.cacheCreate
	details := map[string]any{"cached_tokens": u.cacheRead}
	if u.cacheCreate > 0 {
		details["cache_write_tokens"] = u.cacheCreate
	}
	return map[string]any{
		"input_tokens":          in,
		"input_tokens_details":  details,
		"output_tokens":         u.output,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          in + u.output,
	}
}

// responsesStatus maps an Anthropic stop_reason to (status, incomplete reason).
// stop_sequence is a normal completion; refusal stays "completed" (the refusal
// text is delivered as ordinary output text, as the reference fixtures do).
func responsesStatus(stop string) (status, reason string) {
	switch stop {
	case "max_tokens":
		return "incomplete", "max_output_tokens"
	case "pause_turn":
		return "incomplete", "pause_turn"
	}
	return "completed", ""
}

func responseObject(id, model, status string, createdAt int64, output []any, usage map[string]any, reason string, errObj any) map[string]any {
	r := map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           createdAt,
		"model":                model,
		"status":               status,
		"output":               output,
		"error":                errObj,
		"incomplete_details":   nil,
		"instructions":         nil,
		"metadata":             nil,
		"previous_response_id": nil,
		"temperature":          nil,
		"top_p":                nil,
	}
	if reason != "" {
		r["incomplete_details"] = map[string]any{"reason": reason}
	}
	if usage != nil {
		r["usage"] = usage
	}
	return r
}

func reasoningItem(id, text, encrypted, status string) map[string]any {
	summary := []any{}
	if text != "" {
		summary = append(summary, map[string]any{"type": "summary_text", "text": text})
	}
	item := map[string]any{"type": "reasoning", "id": id, "summary": summary}
	if encrypted != "" {
		item["encrypted_content"] = encrypted
	}
	return item
}

func messageItem(id string, texts []string, status string) map[string]any {
	parts := make([]any, 0, len(texts))
	for _, t := range texts {
		parts = append(parts, outputTextPart(t))
	}
	return map[string]any{"type": "message", "id": id, "role": "assistant", "status": status, "content": parts}
}

func outputTextPart(text string) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
}

func functionCallItem(id, callID, name, args, status string) map[string]any {
	ns, short := splitToolNamespace(name)
	item := map[string]any{
		"type": "function_call", "id": id, "call_id": callID,
		"name": short, "arguments": args, "status": status,
	}
	if ns != "" {
		item["namespace"] = ns
	}
	return item
}

// ConvertAnthropicResponseToResponses converts a non-streaming Anthropic
// Messages response body into a Responses API response body.
func ConvertAnthropicResponseToResponses(body []byte) ([]byte, error) {
	v, err := decodeJSON(body)
	if err != nil {
		return nil, fmt.Errorf("invalid Anthropic response JSON: %w", err)
	}
	msg, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("invalid Anthropic response: JSON object expected")
	}
	if str(msg["type"]) == "error" {
		return nil, errors.New("upstream returned an error object instead of a message")
	}

	id := str(msg["id"])
	output := []any{}
	var texts []string
	flushText := func() {
		if len(texts) == 0 {
			return
		}
		output = append(output, messageItem(derivedID("msg_", id, len(output)), texts, "completed"))
		texts = nil
	}

	blocks, _ := msg["content"].([]any)
	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch str(b["type"]) {
		case "text":
			texts = append(texts, str(b["text"]))
		case "thinking":
			flushText()
			output = append(output, reasoningItem(derivedID("rs_", id, len(output)),
				str(b["thinking"]), str(b["signature"]), "completed"))
		case "redacted_thinking":
			flushText()
			output = append(output, reasoningItem(derivedID("rs_", id, len(output)), "", redactedMarker+str(b["data"]), "completed"))
		case "tool_use":
			flushText()
			args := "{}"
			if in := b["input"]; in != nil {
				if j, err := marshalNoEscape(in); err == nil {
					args = string(j)
				}
			}
			output = append(output, functionCallItem(derivedID("fc_", id, len(output)),
				str(b["id"]), str(b["name"]), args, "completed"))
		}
	}
	flushText()

	status, reason := responsesStatus(str(msg["stop_reason"]))
	usage := parseAnthropicUsage(msg["usage"]).responsesUsage()
	resp := responseObject(id, str(msg["model"]), status, nowUnix(), output, usage, reason, nil)
	return marshalNoEscape(resp)
}

// ResponsesStreamEvent is one serialized Responses SSE event.
type ResponsesStreamEvent struct {
	Type string
	Data []byte // JSON
}

// AnthropicToResponsesStream converts an Anthropic Messages SSE event stream
// into Responses API stream events, event by event. It is tolerant: unknown
// events and missing usage fields never abort the stream.
type AnthropicToResponsesStream struct {
	id, model string
	createdAt int64
	seq       int
	started   bool
	done      bool

	usage      anthropicUsage
	stopReason string
	sawStop    bool

	output     []any // completed output items, in order
	blocks     map[int]*streamBlock
	nextOutput int
	idSeed     string
}

type streamBlock struct {
	kind        string // text, thinking, redacted, tool_use, other
	outputIndex int
	itemID      string
	text        strings.Builder // text or thinking text
	sig         strings.Builder
	args        strings.Builder
	callID      string
	name        string
	closed      bool
}

// NewAnthropicToResponsesStream creates a converter.
func NewAnthropicToResponsesStream() *AnthropicToResponsesStream {
	return &AnthropicToResponsesStream{blocks: map[int]*streamBlock{}, createdAt: nowUnix()}
}

// Done reports whether a terminal event has been emitted.
func (s *AnthropicToResponsesStream) Done() bool { return s.done }

func (s *AnthropicToResponsesStream) emit(typ string, fields map[string]any) ResponsesStreamEvent {
	fields["type"] = typ
	fields["sequence_number"] = s.seq
	s.seq++
	b, _ := marshalNoEscape(fields)
	return ResponsesStreamEvent{Type: typ, Data: b}
}

func (s *AnthropicToResponsesStream) snapshot(status string, final bool, reason string, errObj any) map[string]any {
	out := s.output
	if out == nil {
		out = []any{}
	}
	var usage map[string]any
	if final || s.started {
		usage = s.usage.responsesUsage()
	}
	return responseObject(s.id, s.model, status, s.createdAt, out, usage, reason, errObj)
}

func (s *AnthropicToResponsesStream) start(out *[]ResponsesStreamEvent) {
	if s.started {
		return
	}
	s.started = true
	if s.id == "" {
		s.id = derivedID("resp_", "stream", 0)
	}
	s.idSeed = s.id
	*out = append(*out,
		s.emit("response.created", map[string]any{"response": s.snapshot("in_progress", false, "", nil)}),
		s.emit("response.in_progress", map[string]any{"response": s.snapshot("in_progress", false, "", nil)}),
	)
}

// Feed processes one Anthropic SSE event (the JSON after "data:").
func (s *AnthropicToResponsesStream) Feed(data []byte) []ResponsesStreamEvent {
	if s.done {
		return nil
	}
	v, err := decodeJSON(data)
	if err != nil {
		return nil
	}
	ev, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	var out []ResponsesStreamEvent

	switch str(ev["type"]) {
	case "message_start":
		if m, ok := ev["message"].(map[string]any); ok {
			s.id = str(m["id"])
			s.model = str(m["model"])
			s.usage = parseAnthropicUsage(m["usage"])
		}
		s.start(&out)

	case "content_block_start":
		s.start(&out)
		idx, _ := toInt(ev["index"])
		cb, _ := ev["content_block"].(map[string]any)
		out = append(out, s.blockStart(idx, cb)...)

	case "content_block_delta":
		s.start(&out)
		idx, _ := toInt(ev["index"])
		d, _ := ev["delta"].(map[string]any)
		out = append(out, s.blockDelta(idx, d)...)

	case "content_block_stop":
		idx, _ := toInt(ev["index"])
		out = append(out, s.blockStop(idx)...)

	case "message_delta":
		s.start(&out)
		if d, ok := ev["delta"].(map[string]any); ok {
			if sr := str(d["stop_reason"]); sr != "" {
				s.stopReason = sr
				s.sawStop = true
			}
		}
		s.usage.merge(ev["usage"])

	case "message_stop":
		s.start(&out)
		out = append(out, s.finish()...)

	case "error":
		s.start(&out)
		out = append(out, s.fail(ev["error"])...)
	}
	return out
}

// Finish must be called when the upstream stream ends. If no terminal event was
// produced yet, it completes (when a stop reason was seen) or fails the response.
func (s *AnthropicToResponsesStream) Finish() []ResponsesStreamEvent {
	if s.done {
		return nil
	}
	var out []ResponsesStreamEvent
	s.start(&out)
	if s.sawStop {
		return append(out, s.finish()...)
	}
	return append(out, s.fail(map[string]any{
		"type": "api_error", "message": "upstream stream ended unexpectedly"})...)
}

func (s *AnthropicToResponsesStream) blockStart(idx int, cb map[string]any) []ResponsesStreamEvent {
	var out []ResponsesStreamEvent
	if cb == nil {
		return nil
	}
	b := &streamBlock{outputIndex: -1}
	switch str(cb["type"]) {
	case "text":
		b.kind = "text"
		b.outputIndex = s.nextOutput
		s.nextOutput++
		b.itemID = derivedID("msg_", s.idSeed, b.outputIndex)
		if t := str(cb["text"]); t != "" {
			b.text.WriteString(t)
		}
		out = append(out,
			s.emit("response.output_item.added", map[string]any{
				"output_index": b.outputIndex, "item": messageItem(b.itemID, nil, "in_progress")}),
			s.emit("response.content_part.added", map[string]any{
				"item_id": b.itemID, "output_index": b.outputIndex, "content_index": 0,
				"part": outputTextPart("")}),
		)
		if b.text.Len() > 0 {
			out = append(out, s.textDelta(b, b.text.String()))
		}
	case "thinking":
		b.kind = "thinking"
		b.outputIndex = s.nextOutput
		s.nextOutput++
		b.itemID = derivedID("rs_", s.idSeed, b.outputIndex)
		b.text.WriteString(str(cb["thinking"]))
		out = append(out,
			s.emit("response.output_item.added", map[string]any{
				"output_index": b.outputIndex, "item": reasoningItem(b.itemID, "", "", "in_progress")}),
			s.emit("response.reasoning_summary_part.added", map[string]any{
				"item_id": b.itemID, "output_index": b.outputIndex, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": ""}}),
		)
		if b.text.Len() > 0 {
			out = append(out, s.summaryDelta(b, b.text.String()))
		}
	case "redacted_thinking":
		b.kind = "redacted"
		b.outputIndex = s.nextOutput
		s.nextOutput++
		b.itemID = derivedID("rs_", s.idSeed, b.outputIndex)
		b.sig.WriteString(redactedMarker + str(cb["data"]))
		out = append(out, s.emit("response.output_item.added", map[string]any{
			"output_index": b.outputIndex, "item": reasoningItem(b.itemID, "", "", "in_progress")}))
	case "tool_use":
		b.kind = "tool_use"
		b.outputIndex = s.nextOutput
		s.nextOutput++
		b.itemID = derivedID("fc_", s.idSeed, b.outputIndex)
		b.callID = str(cb["id"])
		b.name = str(cb["name"])
		if in, ok := cb["input"].(map[string]any); ok && len(in) > 0 {
			if j, err := marshalNoEscape(in); err == nil {
				b.args.Write(j)
			}
		}
		item := functionCallItem(b.itemID, b.callID, b.name, "", "in_progress")
		out = append(out, s.emit("response.output_item.added", map[string]any{
			"output_index": b.outputIndex, "item": item}))
	default:
		b.kind = "other" // server tools, citations... not representable
	}
	s.blocks[idx] = b
	return out
}

func (s *AnthropicToResponsesStream) textDelta(b *streamBlock, d string) ResponsesStreamEvent {
	return s.emit("response.output_text.delta", map[string]any{
		"item_id": b.itemID, "output_index": b.outputIndex, "content_index": 0,
		"delta": d, "logprobs": []any{}})
}

func (s *AnthropicToResponsesStream) summaryDelta(b *streamBlock, d string) ResponsesStreamEvent {
	return s.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": b.itemID, "output_index": b.outputIndex, "summary_index": 0, "delta": d})
}

func (s *AnthropicToResponsesStream) blockDelta(idx int, d map[string]any) []ResponsesStreamEvent {
	b := s.blocks[idx]
	if b == nil || d == nil {
		return nil
	}
	switch str(d["type"]) {
	case "text_delta":
		if b.kind == "text" {
			t := str(d["text"])
			b.text.WriteString(t)
			return []ResponsesStreamEvent{s.textDelta(b, t)}
		}
	case "thinking_delta":
		if b.kind == "thinking" {
			t := str(d["thinking"])
			b.text.WriteString(t)
			return []ResponsesStreamEvent{s.summaryDelta(b, t)}
		}
	case "signature_delta":
		if b.kind == "thinking" {
			b.sig.WriteString(str(d["signature"]))
		}
	case "input_json_delta":
		if b.kind == "tool_use" {
			frag := str(d["partial_json"])
			b.args.WriteString(frag)
			return []ResponsesStreamEvent{s.emit("response.function_call_arguments.delta", map[string]any{
				"item_id": b.itemID, "output_index": b.outputIndex, "delta": frag})}
		}
	}
	return nil
}

func (s *AnthropicToResponsesStream) blockStop(idx int) []ResponsesStreamEvent {
	b := s.blocks[idx]
	if b == nil || b.closed {
		return nil
	}
	b.closed = true
	var out []ResponsesStreamEvent
	switch b.kind {
	case "text":
		text := b.text.String()
		out = append(out,
			s.emit("response.output_text.done", map[string]any{
				"item_id": b.itemID, "output_index": b.outputIndex, "content_index": 0,
				"text": text, "logprobs": []any{}}),
			s.emit("response.content_part.done", map[string]any{
				"item_id": b.itemID, "output_index": b.outputIndex, "content_index": 0,
				"part": outputTextPart(text)}),
		)
		item := messageItem(b.itemID, []string{text}, "completed")
		out = append(out, s.itemDone(b, item))
	case "thinking":
		text := b.text.String()
		out = append(out,
			s.emit("response.reasoning_summary_text.done", map[string]any{
				"item_id": b.itemID, "output_index": b.outputIndex, "summary_index": 0, "text": text}),
			s.emit("response.reasoning_summary_part.done", map[string]any{
				"item_id": b.itemID, "output_index": b.outputIndex, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": text}}),
		)
		out = append(out, s.itemDone(b, reasoningItem(b.itemID, text, b.sig.String(), "completed")))
	case "redacted":
		out = append(out, s.itemDone(b, reasoningItem(b.itemID, "", b.sig.String(), "completed")))
	case "tool_use":
		args := b.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out,
			s.emit("response.function_call_arguments.done", map[string]any{
				"item_id": b.itemID, "output_index": b.outputIndex, "arguments": args}),
			s.itemDone(b, functionCallItem(b.itemID, b.callID, b.name, args, "completed")),
		)
	}
	return out
}

func (s *AnthropicToResponsesStream) itemDone(b *streamBlock, item map[string]any) ResponsesStreamEvent {
	s.output = append(s.output, item)
	return s.emit("response.output_item.done", map[string]any{"output_index": b.outputIndex, "item": item})
}

// closeOpen finishes blocks the upstream never closed, in output order.
func (s *AnthropicToResponsesStream) closeOpen() []ResponsesStreamEvent {
	var out []ResponsesStreamEvent
	for idx := 0; idx < len(s.blocks)+s.nextOutput+8; idx++ {
		if b := s.blocks[idx]; b != nil && !b.closed {
			out = append(out, s.blockStop(idx)...)
		}
	}
	return out
}

func (s *AnthropicToResponsesStream) finish() []ResponsesStreamEvent {
	out := s.closeOpen()
	status, reason := responsesStatus(s.stopReason)
	typ := "response.completed"
	if status == "incomplete" {
		typ = "response.incomplete"
	}
	s.done = true
	out = append(out, s.emit(typ, map[string]any{"response": s.snapshot(status, true, reason, nil)}))
	return out
}

func (s *AnthropicToResponsesStream) fail(errv any) []ResponsesStreamEvent {
	out := s.closeOpen()
	code, message := "api_error", "upstream error"
	if m, ok := errv.(map[string]any); ok {
		if t := str(m["type"]); t != "" {
			code = t
		}
		if t := str(m["message"]); t != "" {
			message = t
		}
	}
	s.done = true
	errObj := map[string]any{"code": code, "message": message}
	out = append(out, s.emit("response.failed", map[string]any{"response": s.snapshot("failed", true, "", errObj)}))
	return out
}
