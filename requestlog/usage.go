package requestlog

import (
	"bytes"
	"encoding/json"
)

// UsageScanner pulls token accounting out of an upstream response as it streams.
//
// This is the piece that makes token statistics possible at all: the logging
// transport used to keep only the first 8 KB of a response, but a streamed turn
// reports its usage at the very end (Anthropic puts it in message_delta, OpenAI
// in the final chunk, Responses in response.completed). So the bytes have to be
// watched on their way to the client.
//
// It is deliberately tolerant: any line that does not parse, or carries no
// usage, is ignored. Nothing here may ever fail a request.
type UsageScanner struct {
	line     []byte
	maxLine  int
	overflow bool

	input      int
	output     int
	cacheRead  int
	cacheWrite int
}

// maxUsageLine bounds the per-event buffer. A single SSE event larger than this
// cannot be a usage report, so scanning it is pointless anyway.
const maxUsageLine = 1 << 20

// NewUsageScanner returns a scanner ready to consume a response body.
func NewUsageScanner() *UsageScanner {
	return &UsageScanner{maxLine: maxUsageLine}
}

// Write consumes bytes as they pass through. It never returns an error: the
// response must reach the client even if scanning is abandoned.
func (s *UsageScanner) Write(p []byte) {
	if s.overflow {
		// The current line is already past the cap; look for its end only.
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			s.overflow = false
			s.line = s.line[:0]
			s.scanLine(p[i+1:])
		}
		return
	}
	s.scanLine(p)
}

func (s *UsageScanner) scanLine(p []byte) {
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(s.line)+len(p) > s.maxLine {
				s.overflow = true
				s.line = s.line[:0]
				return
			}
			s.line = append(s.line, p...)
			return
		}
		s.line = append(s.line, p[:i]...)
		s.parseLine(s.line)
		s.line = s.line[:0]
		p = p[i+1:]
	}
}

// Finish parses a trailing line that had no newline (a buffered JSON body).
func (s *UsageScanner) Finish() {
	if len(s.line) > 0 && !s.overflow {
		s.parseLine(s.line)
		s.line = s.line[:0]
	}
}

func (s *UsageScanner) parseLine(line []byte) {
	// One allocation-free check keeps scanning off the hot path for the
	// overwhelming majority of events, which carry only deltas.
	if !bytes.Contains(line, []byte("usage")) {
		return
	}
	payload := line
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	var event any
	if err := json.Unmarshal(payload, &event); err != nil {
		return
	}
	s.walk(event, 0)
}

// walk finds usage objects at any depth: Anthropic nests them under message,
// Responses under response.
func (s *UsageScanner) walk(value any, depth int) {
	if depth > 6 {
		return
	}
	switch node := value.(type) {
	case map[string]any:
		if raw, ok := node["usage"]; ok {
			if usage, ok := raw.(map[string]any); ok {
				s.absorb(usage)
			}
		}
		for _, child := range node {
			switch child.(type) {
			case map[string]any, []any:
				s.walk(child, depth+1)
			}
		}
	case []any:
		for _, child := range node {
			s.walk(child, depth+1)
		}
	}
}

// absorb merges one usage object. Counts are cumulative wherever they repeat, so
// the maximum seen is the truth; this also survives a provider that omits the
// prompt count on the final event.
func (s *UsageScanner) absorb(usage map[string]any) {
	number := func(key string) int {
		if value, ok := usage[key].(float64); ok && value > 0 {
			return int(value)
		}
		return 0
	}
	take := func(dst *int, value int) {
		if value > *dst {
			*dst = value
		}
	}

	// Anthropic and Responses use input_tokens/output_tokens; OpenAI chat uses
	// prompt_tokens/completion_tokens.
	take(&s.input, number("input_tokens"))
	take(&s.input, number("prompt_tokens"))
	take(&s.output, number("output_tokens"))
	take(&s.output, number("completion_tokens"))
	take(&s.cacheRead, number("cache_read_input_tokens"))
	take(&s.cacheWrite, number("cache_creation_input_tokens"))

	// Nested cache detail: OpenAI prompt_tokens_details.cached_tokens and
	// Responses input_tokens_details.cached_tokens.
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if detail, ok := usage[key].(map[string]any); ok {
			if value, ok := detail["cached_tokens"].(float64); ok && value > 0 {
				take(&s.cacheRead, int(value))
			}
		}
	}
}

// Result returns the accumulated counts.
func (s *UsageScanner) Result() (input, output, cacheRead, cacheWrite int) {
	if s == nil {
		return 0, 0, 0, 0
	}
	return s.input, s.output, s.cacheRead, s.cacheWrite
}
