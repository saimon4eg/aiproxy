package providers

// The captured wire bodies, used to prove the one thing a single request cannot
// show: what changes between consecutive turns. Native writes its own working
// directory into the environment block, so a per-request directory moves the cached
// prefix every turn. This test states the invariant on the real material — the only
// content that varies between two turns is that directory — and then checks that with
// the directory held constant the earlier turn's cached prefix recurred in full.
//
// Run with REAL_WIRE_DIR=<dir of cli→relay jsonl captures>; skipped otherwise.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

var requestDir = regexp.MustCompile(`/tmp/claude-cli-\d+`)

func TestRealWireTurnsDifferOnlyInTheRequestDirectory(t *testing.T) {
	dir := os.Getenv("REAL_WIRE_DIR")
	if dir == "" {
		t.Skip("REAL_WIRE_DIR not set")
	}
	bodies := capturedBodies(t, dir, "relay→upstream")
	if len(bodies) < 2 {
		t.Skipf("need at least two captured turns, have %d", len(bodies))
	}

	normalised := func(s string) string { return requestDir.ReplaceAllString(s, "/tmp/claude-cli-shared") }
	cwdBreaks, checked, clean := 0, 0, 0
	for i := 0; i+1 < len(bodies); i++ {
		first, second := bodies[i], bodies[i+1]
		a := flatBlocks(t, first.body)
		b := flatBlocks(t, second.body)

		// The cache write of the earlier turn ends at its last directive.
		writeEnd := -1
		for k, block := range a {
			if block.marked {
				writeEnd = k
			}
		}
		if writeEnd < 0 {
			t.Fatalf("%s caches nothing in `messages`", first.name)
		}

		// Any divergence inside the cached prefix must be native's working directory
		// and nothing else: anything else would be a second, independent way to lose
		// the cache, and this test would then be pinning the wrong cause.
		diverged := -1
		for k := 0; k <= writeEnd && k < len(b); k++ {
			if a[k].plain == b[k].plain {
				continue
			}
			if normalised(a[k].plain) != normalised(b[k].plain) {
				t.Fatalf("%s → %s: cached prefix diverges at block %d (%s) in something other than the working directory:\n  %s\n  %s",
					first.name, second.name, k, a[k].label, shorten(a[k].plain, 240), shorten(b[k].plain, 240))
			}
			diverged = k
		}
		if diverged < 0 {
			clean++
			t.Logf("%s → %s: cached prefix = %d blocks, replayed as sent — nothing inside it varies",
				first.name, second.name, writeEnd+1)
			continue
		}
		checked++
		cwdBreaks++

		// With the directory held constant — what the shared workdir does — the whole
		// cached prefix recurs, byte for byte.
		for k := 0; k <= writeEnd && k < len(b); k++ {
			if normalised(a[k].plain) != normalised(b[k].plain) {
				t.Fatalf("%s → %s: prefix still diverges at block %d (%s) with the directory normalised",
					first.name, second.name, k, a[k].label)
			}
		}
		t.Logf("%s → %s: cached prefix = %d blocks, broken at %s by the working directory alone, and it recurs in full once the cwd is constant",
			first.name, second.name, writeEnd+1, a[diverged].label)
	}
	// Both states are valid material for this test: a capture from before the shared
	// workdir lands in `cwdBreaks`, one from after lands in `clean`. What is NOT valid
	// is a body whose prefix diverges in something other than the directory — that is
	// fatal above, because it would mean a second, independent way to lose the cache.
	if checked == 0 {
		t.Logf("every captured turn pair replays its cached prefix as sent (%d pairs): this capture was taken with the shared workdir in place, so there is no directory divergence left to explain", clean)
	} else {
		t.Logf("%d of %d turn pairs had their cached prefix broken by the per-request directory; %d already clean",
			cwdBreaks, cwdBreaks+clean, clean)
	}
}

type capturedBody struct {
	name string
	body map[string]any
}

// capturedBodies reads the bodies of one direction from a log directory, in the order
// the requests were made.
func capturedBodies(t *testing.T, dir, direction string) []capturedBody {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	sort.Strings(files)
	var out []capturedBody
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range splitJSONLines(raw) {
			var row map[string]any
			if json.Unmarshal(line, &row) != nil || row["dir"] != direction {
				continue
			}
			text, _ := row["body"].(string)
			if text == "" {
				continue
			}
			var body map[string]any
			if json.Unmarshal([]byte(text), &body) != nil {
				continue
			}
			out = append(out, capturedBody{name: filepath.Base(file), body: body})
		}
	}
	return out
}

type flatBlock struct {
	label  string
	plain  string // content without any cache directive
	marked bool
}

// flatBlocks renders a body as the ordered block list a cache prefix is compared
// against: tools, then system, then every message block.
func flatBlocks(t *testing.T, body map[string]any) []flatBlock {
	t.Helper()
	var out []flatBlock
	add := func(label string, value any, marked bool) {
		encoded, err := json.Marshal(plainValue(value))
		if err != nil {
			t.Fatalf("encode %s: %v", label, err)
		}
		out = append(out, flatBlock{label: label, plain: string(encoded), marked: marked})
	}
	for _, key := range []string{"tools", "system"} {
		blocks, _ := body[key].([]any)
		for i, block := range blocks {
			mapped, _ := block.(map[string]any)
			_, marked := mapped["cache_control"]
			add(key+"["+itoa(i)+"]", block, marked)
		}
	}
	messages, _ := body["messages"].([]any)
	for n, raw := range messages {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		switch content := message["content"].(type) {
		case string:
			add("m"+itoa(n)+"("+role+")", content, false)
		case []any:
			for j, block := range content {
				mapped, _ := block.(map[string]any)
				_, marked := mapped["cache_control"]
				add("m"+itoa(n)+"("+role+")["+itoa(j)+"]", block, marked)
			}
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [8]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
