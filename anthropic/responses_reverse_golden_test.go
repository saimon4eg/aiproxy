package anthropic

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Golden tests: the native converters are compared with the committed fixtures
// in anthropic/testdata/reference. Every intentional difference is listed in
// refDeviation* below and explained in testdata/reference/DIFFERENCES.md;
// anything else must match exactly (after normalising random ids, timestamps
// and nulls).

const refDir = "testdata/reference"

var generatedID = regexp.MustCompile(`^(rs|msg|fc)_[0-9a-f]{48,}$`)

func readJSONFile(t *testing.T, path string) any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	v, err := decodeJSON(b)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return v
}

// norm canonicalises a decoded JSON value for comparison.
func norm(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if val == nil || k == "created_at" {
				continue
			}
			nv := norm(val)
			if k == "arguments" {
				if s, ok := nv.(string); ok {
					if p, err := decodeJSON([]byte(s)); err == nil {
						nv = norm(p)
					}
				}
			}
			out[k] = nv
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = norm(e)
		}
		return out
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		if m := generatedID.FindStringSubmatch(x); m != nil {
			return "<" + m[1] + "_id>"
		}
	}
	return v
}

func names(t *testing.T, kind string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(refDir, kind, "*.in.json"))
	var res []string
	for _, f := range files {
		res = append(res, strings.TrimSuffix(filepath.Base(f), ".in.json"))
	}
	sort.Strings(res)
	if len(res) == 0 {
		t.Fatalf("no fixtures in %s/%s", refDir, kind)
	}
	return res
}

func fixtureError(out any) (string, bool) {
	m, ok := out.(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := m["error"].(string)
	return s, ok
}

// refDeviationReq lists request fixtures where the native converter deliberately
// differs from the reference. Each entry is asserted separately in
// TestRequestDeviations; here they are only excluded from the exact comparison.
var refDeviationReq = map[string]string{
	"instructions":                "instructions -> system (the reference drops them)",
	"instructions_and_system":     "instructions kept together with input system messages",
	"everything":                  "instructions -> system",
	"invalid_no_input":            "both reject; error text differs",
	"refusal_in_assistant":        "refusal parts are accepted and replayed as text",
	"reasoning_summary_none":      "summary:none accepted (no summary requested)",
	"tool_local_shell":            "unsupported tool dropped with warning instead of failing the request",
	"tool_file_search":            "unsupported tool dropped with warning instead of failing the request",
	"tool_function_nested":        "Chat-style nested function tool accepted",
	"tool_choice_function_nested": "nested function tool_choice honoured (the reference drops it)",
	"tool_custom":                 "no empty tools array is sent",
	"service_tier":                "service_tier not forwarded (value mapping is provider specific)",
	"user_field":                  "user -> metadata.user_id",
	"empty_user_text":             "empty text parts are skipped (Anthropic rejects empty text blocks)",
	"reasoning_empty_summary":     "empty-summary reasoning is replayed as a signed thinking block, not redacted_thinking",
	"max_tokens_zero_default":     "both reject; error text differs",
	"function_call_array_args":    "non-object arguments are sent as {} (Anthropic tool_use.input must be an object)",
	"input_file_data":             "data: URL prefix is stripped from file_data (the reference leaves it in the base64 payload)",
	"parallel_tool_calls_false":   "parallel_tool_calls:false -> disable_parallel_tool_use (the reference drops it)",
}

func TestResponsesRequestGolden(t *testing.T) {
	for _, name := range names(t, "req") {
		name := name
		t.Run(name, func(t *testing.T) {
			if why, skip := refDeviationReq[name]; skip {
				t.Skip("intentional deviation: " + why)
			}
			inBytes, _ := os.ReadFile(filepath.Join(refDir, "req", name+".in.json"))
			want := readJSONFile(t, filepath.Join(refDir, "req", name+".out.json"))
			got, _, err := ConvertResponsesRequestToAnthropic(inBytes)
			if msg, isErr := fixtureError(want); isErr {
				if err == nil {
					t.Fatalf("the reference rejects this input (%s) but native accepted it", msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("native error: %v", err)
			}
			gotV, _ := decodeJSON(got)
			if !reflect.DeepEqual(norm(gotV), norm(want)) {
				g, _ := marshalNoEscape(norm(gotV))
				w, _ := marshalNoEscape(norm(want))
				t.Fatalf("mismatch\n got: %s\nwant: %s", g, w)
			}
		})
	}
}

var refDeviationResp = map[string]string{
	"stop_stop_sequence":    "stop_sequence is a normal completion, not incomplete",
	"thinking_no_signature": "unsigned thinking is kept as a reasoning summary (the reference drops it)",
	"redacted_thinking":     "redacted_thinking data carries a marker so it can be replayed as redacted_thinking",
}

func TestResponsesResponseGolden(t *testing.T) {
	for _, name := range names(t, "resp") {
		name := name
		t.Run(name, func(t *testing.T) {
			if why, skip := refDeviationResp[name]; skip {
				t.Skip("intentional deviation: " + why)
			}
			inBytes, _ := os.ReadFile(filepath.Join(refDir, "resp", name+".in.json"))
			want := readJSONFile(t, filepath.Join(refDir, "resp", name+".out.json"))
			got, err := ConvertAnthropicResponseToResponses(inBytes)
			if err != nil {
				t.Fatalf("native error: %v", err)
			}
			gotV, _ := decodeJSON(got)
			if !reflect.DeepEqual(norm(gotV), norm(want)) {
				g, _ := marshalNoEscape(norm(gotV))
				w, _ := marshalNoEscape(norm(want))
				t.Fatalf("mismatch\n got: %s\nwant: %s", g, w)
			}
		})
	}
}

// Streams whose expected output differs from the reference fixtures on purpose. They get
// dedicated assertions in the stream tests below.
var refDeviationStream = map[string]string{
	"thinking_text":                       "reasoning items are streamed (the reference drops thinking)",
	"thinking_no_signature":               "reasoning items are streamed",
	"thinking_text_tool":                  "reasoning items are streamed",
	"thinking_only":                       "reasoning items are streamed",
	"thinking_multi_delta_sig_split":      "reasoning items are streamed",
	"tool_use_no_args":                    "empty tool arguments are normalised to {}",
	"error_midstream":                     "failed response carries usage",
	"tolerant_delta_without_input_tokens": "the reference fails on this input; native must not",
	"tolerant_unknown_event":              "the reference fails on this input; native must not",
	"max_tokens":                          "item status stays completed (stop reason is only known after the block closed); the response itself is incomplete",
	"no_message_delta":                    "response.completed carries usage from message_start",
	"ping_events":                         "ping is not forwarded (the reference emits a non-standard keepalive event)",
}

func runStream(t *testing.T, events []any) []map[string]any {
	t.Helper()
	conv := NewAnthropicToResponsesStream()
	var out []map[string]any
	collect := func(evs []ResponsesStreamEvent) {
		for _, e := range evs {
			v, err := decodeJSON(e.Data)
			if err != nil {
				t.Fatalf("event %s is not valid JSON: %v", e.Type, err)
			}
			out = append(out, v.(map[string]any))
		}
	}
	for _, ev := range events {
		b, _ := marshalNoEscape(ev)
		collect(conv.Feed(b))
	}
	collect(conv.Finish())
	return out
}

func TestResponsesStreamGolden(t *testing.T) {
	for _, name := range names(t, "stream") {
		name := name
		t.Run(name, func(t *testing.T) {
			if why, skip := refDeviationStream[name]; skip {
				t.Skip("intentional deviation: " + why)
			}
			in := readJSONFile(t, filepath.Join(refDir, "stream", name+".in.json")).([]any)
			want := readJSONFile(t, filepath.Join(refDir, "stream", name+".out.json"))
			if _, isErr := fixtureError(want); isErr {
				t.Fatal("fixture is an error but is not listed as a deviation")
			}
			got := runStream(t, in)
			gotAny := make([]any, len(got))
			for i, g := range got {
				gotAny[i] = g
			}
			if !reflect.DeepEqual(norm(gotAny), norm(want)) {
				t.Fatal(firstStreamDiff(norm(gotAny).([]any), norm(want).([]any)))
			}
		})
	}
}

// firstStreamDiff reports the first differing event of two normalised streams.
func firstStreamDiff(got, want []any) string {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if !reflect.DeepEqual(got[i], want[i]) {
			g, _ := marshalNoEscape(got[i])
			w, _ := marshalNoEscape(want[i])
			return fmt.Sprintf("event %d differs\n got: %s\nwant: %s", i, g, w)
		}
	}
	return fmt.Sprintf("event count differs: got %d, want %d", len(got), len(want))
}
