package requestlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"unicode/utf8"
)

// Body logs are a diagnostic channel, not an archive.
//
// The volume comes from one shape: a Messages request carries `model`, `system` and
// `tools` before the conversation, and the conversation itself runs to hundreds of
// kilobytes. Keeping only the first N bytes therefore shows everything except the
// part worth reading — the newest turns and the cache directives sitting on them —
// while keeping all N bytes pays disk for material nobody reads twice.
//
// So both ends are kept and the middle is replaced by a marker that states how much
// was dropped. For a JSON body the cut is structural rather than byte-wise: bulky
// arrays keep their first and last entries, long strings keep their ends, and the
// line stays parseable so the log can still be read by a script instead of by eye.

const (
	// defaultBodyLimit is the per-line ceiling. Two lines per request carry a
	// request body and one carries the response, so a heavily loaded day stays in
	// the tens of megabytes rather than the hundreds.
	defaultBodyLimit = 32 << 10 // 32 KB
	// markerReserve is the room set aside for the byte count inside a marker, so a
	// budget computed before the count is known still holds once it is.
	markerReserve = 40
	// arrayHead and arrayTail are how many entries of a bulky array survive. The
	// tail carries the newest turns, the head carries the opening context; both
	// matter for the same request.
	arrayHead = 2
	arrayTail = 4
	// minStringCap keeps a long text block readable instead of reducing it to a
	// marker: below this a text block is small enough not to be the problem.
	minStringCap = 256
)

// BodyLimit is how many bytes of a request or response body land in one body log
// line. AIPROXY_LOG_BODY_LIMIT overrides the default.
func BodyLimit() int {
	if v, ok := os.LookupEnv("AIPROXY_LOG_BODY_LIMIT"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultBodyLimit
}

// ElideBody renders body for a body log line of at most about limit bytes.
//
// JSON is elided structurally — the result stays valid JSON and every key survives.
// Anything else (an SSE stream, malformed input) is elided byte-wise against the
// same budget.
func ElideBody(body []byte, limit int) string {
	if len(body) == 0 {
		return ""
	}
	if limit < 1024 {
		limit = 1024
	}
	if len(body) <= limit {
		return string(body)
	}
	if slim, ok := elideJSON(body, limit); ok {
		return string(slim)
	}
	return elideBytes(body, limit)
}

// elideJSON cuts a JSON document until it fits, taking the least invasive step that
// works: first only long strings are shortened, and only if that is not enough are
// the arrays themselves thinned. `tools` and `messages` are the parts worth reading,
// so they are the last thing touched.
func elideJSON(body []byte, limit int) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber() // keeps 1234567890123456789 exact instead of rounding it
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	if decoder.More() {
		return nil, false // more than one document: not ours to rearrange
	}
	type attempt struct {
		stringCap int
		arrays    bool
	}
	for _, step := range []attempt{
		{limit / 8, false},
		{limit / 8, true},
		{limit / 32, true},
		{limit / 128, true},
	} {
		cap := step.stringCap
		if cap < minStringCap {
			cap = minStringCap
		}
		encoded, err := json.Marshal(elideValue(value, cap, step.arrays))
		if err != nil {
			return nil, false
		}
		if len(encoded) <= limit {
			return encoded, true
		}
	}
	return nil, false
}

// elideValue walks a decoded JSON value, cutting long strings and — when arrays is
// set — the middle of long arrays. Everything else is returned unchanged.
func elideValue(value any, stringCap int, arrays bool) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = elideValue(item, stringCap, arrays)
		}
		return out
	case []any:
		if !arrays || len(typed) <= arrayHead+arrayTail+2 {
			out := make([]any, len(typed))
			for i, item := range typed {
				out[i] = elideValue(item, stringCap, arrays)
			}
			return out
		}
		kept := make([]any, 0, arrayHead+arrayTail+1)
		for _, item := range typed[:arrayHead] {
			kept = append(kept, elideValue(item, stringCap, arrays))
		}
		dropped := typed[arrayHead : len(typed)-arrayTail]
		kept = append(kept, map[string]any{"_elided": map[string]any{
			"elements": len(dropped),
			"bytes":    encodedSize(dropped),
		}})
		for _, item := range typed[len(typed)-arrayTail:] {
			kept = append(kept, elideValue(item, stringCap, arrays))
		}
		return kept
	case string:
		return elideString(typed, stringCap)
	default:
		return value
	}
}

// elideString keeps the ends of a long string and states how many characters went
// missing. The count is in characters, not bytes, because the ends are cut on rune
// boundaries so the log stays valid UTF-8.
func elideString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	half := (limit - markerReserve) / 2
	if half < 32 {
		half = 32
	}
	head := headBytes(s, half)
	tail := tailBytes(s, half)
	return head + elidedCharsMarker(len(s)-len(head)-len(tail)) + tail
}

// elideBytes is the last-resort cut: the head and the tail of body, with the count
// of the dropped middle between them.
func elideBytes(body []byte, limit int) string {
	if len(body) <= limit {
		return string(body)
	}
	budget := limit - markerReserve
	if budget < 64 {
		limit = 64 + markerReserve
		budget = 64
	}
	headLen := budget / 2
	tailLen := budget - headLen
	cut := len(body) - headLen - tailLen
	var out bytes.Buffer
	out.Grow(headLen + tailLen + markerReserve)
	out.Write(body[:headLen])
	out.WriteString(elidedMarker(cut))
	out.Write(body[len(body)-tailLen:])
	return out.String()
}

// elidedMarker names the size of the cut, so a reader can tell a small request from
// a truncated one without counting bytes.
func elidedMarker(cut int) string {
	return fmt.Sprintf("\n[... elided %d bytes ...]\n", cut)
}

// elidedCharsMarker is the in-string form, used where embedding a newline-heavy
// marker would break the value being quoted.
func elidedCharsMarker(cut int) string {
	return fmt.Sprintf("[... elided %d chars ...]", cut)
}

// encodedSize is how many bytes the dropped entries occupied, for the marker.
func encodedSize(items []any) int {
	total := 0
	for _, item := range items {
		if encoded, err := json.Marshal(item); err == nil {
			total += len(encoded)
		}
	}
	return total
}

// headBytes returns the largest prefix of s that is at most n bytes and does not
// split a rune.
func headBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// tailBytes returns the largest suffix of s that is at most n bytes and does not
// split a rune.
func tailBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	if start < 0 {
		start = 0
	}
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
