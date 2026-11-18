package providers

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClaude speaks the stream-json contract the transport depends on. Its
// behaviour comes from fake.conf.json next to the script, not from environment
// variables, so the test never has to widen the child's env whitelist.
const fakeClaudeScript = `#!/usr/bin/env python3
import json, os, sys, urllib.request

here = os.path.dirname(os.path.abspath(__file__))
cfg = {}
conf_path = os.path.join(here, "fake.conf.json")
if os.path.exists(conf_path):
    with open(conf_path) as fh:
        cfg = json.load(fh)

mode = cfg.get("mode", "text")

if cfg.get("dump_env"):
    with open(cfg["dump_env"], "w") as fh:
        json.dump(dict(os.environ), fh)

if cfg.get("dump_argv"):
    with open(cfg["dump_argv"], "w") as fh:
        json.dump(sys.argv, fh)

if cfg.get("dump_system"):
    # Capture what --system-prompt-file points at, before the CLI would read it.
    args = sys.argv
    if "--system-prompt-file" in args:
        with open(args[args.index("--system-prompt-file") + 1]) as src, open(cfg["dump_system"], "w") as fh:
            fh.write(src.read())

if cfg.get("dump_cwd"):
    # Append: one line per request, so consecutive requests can be compared.
    with open(cfg["dump_cwd"], "a") as fh:
        fh.write(os.getcwd() + "\n")

def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n"); sys.stdout.flush()

def ack():
    emit({"type": "result", "subtype": "success", "num_turns": 0,
          "is_error": False, "usage": {"input_tokens": 1, "output_tokens": 0}})

def call_upstream(times=1):
    """POST to the relay once per attempt; returns the last HTTP status, 0 when none."""
    base = os.environ.get("ANTHROPIC_BASE_URL", "").rstrip("/")
    if not base:
        return 0
    payload = json.dumps({"model": "claude-sonnet-5-5", "max_tokens": 8,
                          "messages": [{"role": "user", "content": "hi"}]}).encode()
    status = 0
    for _ in range(times):
        req = urllib.request.Request(base + "/v1/messages", data=payload,
                                     headers={"content-type": "application/json",
                                              "anthropic-beta": "oauth-2025-04-20"})
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                resp.read()
                status = resp.status
        except urllib.error.HTTPError as refused:
            status = refused.code
        except Exception:
            pass
    return status

def text_answer():
    emit({"type": "stream_event", "event": {"type": "message_start", "message": {
        "id": "msg_fake", "role": "assistant", "content": [],
        "usage": {"input_tokens": 11, "output_tokens": 0}}}})
    emit({"type": "stream_event", "event": {"type": "content_block_start", "index": 0,
          "content_block": {"type": "thinking", "thinking": ""}}})
    emit({"type": "stream_event", "event": {"type": "content_block_delta", "index": 0,
          "delta": {"type": "thinking_delta", "thinking": "let me think"}}})
    emit({"type": "stream_event", "event": {"type": "content_block_delta", "index": 0,
          "delta": {"type": "signature_delta", "signature": "sig-1"}}})
    emit({"type": "stream_event", "event": {"type": "content_block_stop", "index": 0}})
    emit({"type": "stream_event", "event": {"type": "content_block_start", "index": 1,
          "content_block": {"type": "text", "text": ""}}})
    for part in ("Hello", " world"):
        emit({"type": "stream_event", "event": {"type": "content_block_delta", "index": 1,
              "delta": {"type": "text_delta", "text": part}}})
    emit({"type": "stream_event", "event": {"type": "content_block_stop", "index": 1}})
    emit({"type": "stream_event", "event": {"type": "message_stop"}})
    emit({"type": "assistant", "message": {"id": "msg_fake", "role": "assistant",
        "model": "claude-sonnet-5-5", "stop_reason": "end_turn",
        "content": [{"type": "thinking", "thinking": "let me think", "signature": "sig-1"},
                    {"type": "text", "text": "Hello world"}],
        "usage": {"input_tokens": 11, "output_tokens": 7}}})
    emit({"type": "result", "subtype": "success", "num_turns": 1, "is_error": False,
          "usage": {"input_tokens": 11, "output_tokens": 7},
          "total_cost_usd": 0.0123,
          "modelUsage": {"claude-sonnet-5-5": {"costBasis": "list"}}})

def tool_answer():
    emit({"type": "stream_event", "event": {"type": "message_stop"}})
    emit({"type": "assistant", "message": {"id": "msg_tool", "role": "assistant",
        "model": "claude-sonnet-5-5", "stop_reason": "tool_use",
        "content": [{"type": "thinking", "thinking": "need the file", "signature": "sig-tool"},
                    {"type": "text", "text": "checking"},
                    {"type": "tool_use", "id": "toolu_1",
                     "name": "mcp__hermes__read_file", "input": {"path": "a.txt"}}],
        "usage": {"input_tokens": 20, "output_tokens": 9}}})
    emit({"type": "result", "subtype": "success", "num_turns": 1, "is_error": False,
          "usage": {"input_tokens": 20, "output_tokens": 9}})

frames = []
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    frames.append(json.loads(line))
    if cfg.get("dump_frames"):
        with open(cfg["dump_frames"], "a") as fh:
            fh.write(line + "\n")
    if frames[-1].get("shouldQuery") is False:
        ack()

if cfg.get("upstream_calls"):
    upstream_status = call_upstream(cfg["upstream_calls"])
    if upstream_status and cfg.get("report_upstream_error"):
        # What 2.1.311 does when the upstream refuses before any content: the
        # failure rides the event and the result is is_error=true with
        # subtype=success. Only the relay knows the real status.
        emit({"type": "assistant", "message": {"id": "m", "role": "assistant",
            "model": "<synthetic>", "stop_reason": "stop_sequence",
            "content": [{"type": "text", "text": "API Error: Server is temporarily limiting requests (not your usage limit)"}],
            "usage": {"input_tokens": 0, "output_tokens": 0}},
            "error": "overloaded", "is_api_error_message": True, "api_error_status": upstream_status})
        emit({"type": "result", "subtype": "success", "is_error": True, "num_turns": 1,
              "result": "API Error: Server is temporarily limiting requests (not your usage limit)",
              "terminal_reason": "api_error",
              "usage": {"input_tokens": 0, "output_tokens": 0}})
        sys.exit(0)
    if not upstream_status and cfg.get("fail_without_upstream"):
        # What 2.1.311 does when the upstream is unreachable: the failure rides
        # the event and the result is still is_error=true with subtype=success.
        emit({"type": "assistant", "message": {"id": "m", "role": "assistant",
            "model": "<synthetic>", "stop_reason": "stop_sequence",
            "content": [{"type": "text", "text": "API Error: Connection refused"}],
            "usage": {"input_tokens": 0, "output_tokens": 0}},
            "error": "server_error", "is_api_error_message": True, "api_error_status": 503})
        emit({"type": "result", "subtype": "success", "is_error": True, "num_turns": 1,
              "result": "API Error: Connection refused", "terminal_reason": "api_error",
              "usage": {"input_tokens": 0, "output_tokens": 0}})
        sys.exit(0)

if mode == "tool":
    tool_answer()
elif mode == "silent":
    pass
elif mode == "auth_error":
    # Verbatim shape 2.1.311 produces when the container has no login: the code
    # rides the EVENT, and the result is still labelled subtype=success.
    emit({"type": "assistant", "message": {"id": "m", "role": "assistant",
        "model": "<synthetic>", "stop_reason": "stop_sequence",
        "content": [{"type": "text", "text": "Not logged in \u00b7 Please run /login"}],
        "usage": {"input_tokens": 0, "output_tokens": 0}},
        "error": "authentication_failed", "is_api_error_message": True})
    emit({"type": "result", "subtype": "success", "is_error": True, "num_turns": 1,
          "result": "Not logged in", "terminal_reason": "api_error",
          "usage": {"input_tokens": 0, "output_tokens": 0}})
elif mode == "garbage":
    sys.stdout.write("mise WARN tool not activated\n"); sys.stdout.flush()
elif mode == "drop_midstream":
    # Content reached the client, so the relay's 200 is already committed, and only
    # then does the upstream abort with its own prose.
    emit({"type": "stream_event", "event": {"type": "message_start", "message": {
        "id": "msg_cut", "role": "assistant", "content": [],
        "usage": {"input_tokens": 11, "output_tokens": 0}}}})
    emit({"type": "stream_event", "event": {"type": "content_block_start", "index": 0,
          "content_block": {"type": "text", "text": ""}}})
    emit({"type": "stream_event", "event": {"type": "content_block_delta", "index": 0,
          "delta": {"type": "text_delta", "text": "partial"}}})
    emit({"type": "assistant", "message": {"id": "msg_cut", "role": "assistant",
        "model": "<synthetic>", "stop_reason": "stop_sequence",
        "content": [{"type": "text", "text": "API Error: Server is temporarily limiting requests (not your usage limit)"}],
        "usage": {"input_tokens": 0, "output_tokens": 0}},
        "error": "overloaded", "is_api_error_message": True, "api_error_status": 529})
    emit({"type": "result", "subtype": "success", "is_error": True, "num_turns": 1,
          "result": "API Error: Server is temporarily limiting requests (not your usage limit)",
          "terminal_reason": "api_error",
          "usage": {"input_tokens": 0, "output_tokens": 0}})
else:
    text_answer()
`

// fakeClaude is a fake CLI installation: script + its config file.
type fakeClaude struct {
	dir string
	bin string
}

func newFakeClaude(t *testing.T, cfg map[string]any) *fakeClaude {
	t.Helper()
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("/usr/bin/env is missing")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fake.conf.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return &fakeClaude{dir: dir, bin: bin}
}

func (f *fakeClaude) path(name string) string { return filepath.Join(f.dir, name) }

// upstreamRecorder stands in for api.anthropic.com behind the relay.
//
// It is deliberately strict about the request path: a fake that answers on any
// URL let a doubled /v1/messages/v1/messages pass every test, which is exactly
// how that bug reached a live run.
type upstreamRecorder struct {
	mu      sync.Mutex
	hits    int
	bodies  []string
	headers []http.Header
	paths   []string
	queries []string
	// gzip makes the recorder compress its stream, like a real edge does when the
	// client advertises support.
	gzip bool
	// delay is the pause between streamed events, for idle-timeout tests.
	delay time.Duration
}

func (u *upstreamRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var builder strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				builder.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		u.mu.Lock()
		u.hits++
		u.bodies = append(u.bodies, builder.String())
		u.headers = append(u.headers, r.Header.Clone())
		u.paths = append(u.paths, r.URL.Path)
		u.queries = append(u.queries, r.URL.RawQuery)
		compress := u.gzip
		delay := u.delay
		u.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		var emit io.Writer = w
		if compress {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			defer gz.Close()
			emit = gz
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(emit, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":5}}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		fmt.Fprint(emit, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"up\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		fmt.Fprint(emit, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
}

func (u *upstreamRecorder) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

// testProvider builds a claude-cli provider wired to a fake binary with an
// existing (fake) login, so the request path is exercised end to end.
func testProvider(t *testing.T, upstream string, cfg map[string]any) (*ProviderConfig, *fakeClaude) {
	t.Helper()
	fake := newFakeClaude(t, cfg)
	loginDir := filepath.Join(fake.dir, "login")
	if err := os.MkdirAll(loginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loginDir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &ProviderConfig{
		ProviderID:      "claude-test",
		Type:            "claude-cli",
		Auth:            "oauth",
		Enabled:         true,
		BaseURL:         upstream,
		ModelPrefix:     "claude-test",
		Command:         fake.bin,
		ReadIdleSeconds: 20,
		ClaudeConfigDir: loginDir,
	}
	if err := p.initHTTPClient(); err != nil {
		t.Fatal(err)
	}
	return p, fake
}

func post(t *testing.T, p *ProviderConfig, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	p.makeClaudeCLIHandler().ServeHTTP(rec, req)
	return rec
}

const baseBody = `{"model":"claude-sonnet-5-5","max_tokens":64,"stream":true,
 "system":[{"type":"text","text":"You are a test."}],
 "messages":[{"role":"user","content":"hi"}]}`

func TestClaudeCLIStreamsTextAndThinking(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "text"})
	rec := post(t, p, baseBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start", "thinking_delta", "let me think",
		"text_delta", "Hello", "event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	if got := strings.Count(body, "text_delta"); got < 2 {
		t.Fatalf("expected per-delta streaming, got %d text_delta events:\n%s", got, body)
	}
}

func TestClaudeCLINonStreamingResponse(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "text"})
	body := strings.Replace(baseBody, `"stream":true`, `"stream":false`, 1)
	rec := post(t, p, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Type    string `json:"type"`
		Content []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	if response.StopReason != "end_turn" {
		t.Fatalf("stop_reason %q", response.StopReason)
	}
	if response.Usage.InputTokens != 11 || response.Usage.OutputTokens != 7 {
		t.Fatalf("usage %+v", response.Usage)
	}
	var text, thinking string
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			text += block.Text
		case "thinking":
			thinking = block.Thinking
			if block.Signature != "sig-1" {
				t.Fatalf("thinking signature lost: %q", block.Signature)
			}
		}
	}
	if text != "Hello world" {
		t.Fatalf("text %q", text)
	}
	if thinking != "let me think" {
		t.Fatalf("thinking %q", thinking)
	}
}

func TestClaudeCLIToolCallUsesHostName(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "tool"})
	body := strings.Replace(baseBody, `"max_tokens":64`,
		`"tools":[{"name":"read_file","description":"read","input_schema":{"type":"object"}}],"max_tokens":64`, 1)
	body = strings.Replace(body, `"stream":true`, `"stream":false`, 1)
	rec := post(t, p, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name"`
			ID    string         `json:"id"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	if response.StopReason != "tool_use" {
		t.Fatalf("stop_reason %q (%s)", response.StopReason, rec.Body.String())
	}
	var found bool
	for _, block := range response.Content {
		if block.Type != "tool_use" {
			continue
		}
		found = true
		if block.Name != "read_file" {
			t.Fatalf("wire name leaked to the host: %q", block.Name)
		}
		if block.ID != "toolu_1" {
			t.Fatalf("tool_use id lost: %q", block.ID)
		}
		if block.Input["path"] != "a.txt" {
			t.Fatalf("tool input lost: %+v", block.Input)
		}
	}
	if !found {
		t.Fatalf("no tool_use block in: %s", rec.Body.String())
	}
}

// The host's credential variables must never reach the child, and HOME must not
// either: with no ambient login the CLI cannot silently switch accounts.
func TestClaudeCLIChildEnvIsScrubbed(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, fake := testProvider(t, server.URL, map[string]any{
		"mode": "text", "dump_env": "", "dump_argv": "",
	})
	// Paths are only known inside the temp dir, so rewrite the config now.
	writeFakeConfig(t, fake, map[string]any{
		"mode": "text", "dump_env": fake.path("env.json"), "dump_argv": fake.path("argv.json"),
	})

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-should-never-arrive")
	rec := post(t, p, baseBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(fake.path("env.json"))
	if err != nil {
		t.Fatalf("child env was not recorded: %v", err)
	}
	var childEnv map[string]string
	if err := json.Unmarshal(data, &childEnv); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
	} {
		if value, ok := childEnv[forbidden]; ok {
			t.Fatalf("%s leaked into the child environment (value %q)", forbidden, value)
		}
	}
	// HOME must be a directory we own and keep empty, never the operator's: with
	// a real home the CLI could find ~/.claude and run on another account.
	home, ok := childEnv["HOME"]
	if !ok {
		t.Fatal("HOME should point at our own empty directory, not be absent")
	}
	if hostHome := os.Getenv("HOME"); hostHome != "" && home == hostHome {
		t.Fatalf("the operator's HOME reached the child: %q", home)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("the child HOME does not exist: %v", err)
	}
	base := childEnv["ANTHROPIC_BASE_URL"]
	if !strings.Contains(base, "/admit/") {
		t.Fatalf("child was not pointed at the admission route: %q", base)
	}
	if childEnv["CLAUDE_CODE_MAX_RETRIES"] != "0" {
		t.Fatal("native retries must be disabled")
	}
	if childEnv["CLAUDE_CODE_TOTAL_TOKENS_REMINDER"] != "off" {
		t.Fatal("the native token reminder breaks the cached prefix and must stay off")
	}
}

// Every capability that would let native act as an agent is disabled at argv.
func TestClaudeCLIArgvDisablesNativeAgency(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, fake := testProvider(t, server.URL, map[string]any{"mode": "text"})
	writeFakeConfig(t, fake, map[string]any{"mode": "text", "dump_argv": fake.path("argv.json")})

	if rec := post(t, p, baseBody); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(fake.path("argv.json"))
	if err != nil {
		t.Fatalf("argv was not recorded: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(data, &argv); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"--tools  ", "--max-turns 1", "--permission-mode dontAsk",
		"--no-session-persistence", "--strict-mcp-config",
		"--disable-slash-commands", "--include-partial-messages",
		"--setting-sources  ", "--input-format stream-json",
	} {
		if !strings.Contains(joined+" ", want) {
			t.Fatalf("argv missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Fatal("native permissions must never be bypassed")
	}
}

// History replay: historical user frames say shouldQuery=false, the querying
// frame omits the field entirely.
func TestClaudeCLIReplaysHistoryWithAcks(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, fake := testProvider(t, server.URL, map[string]any{"mode": "text"})
	writeFakeConfig(t, fake, map[string]any{"mode": "text", "dump_frames": fake.path("frames.jsonl")})

	body := `{"model":"claude-sonnet-5-5","max_tokens":64,"stream":false,"messages":[
	  {"role":"user","content":"first"},
	  {"role":"assistant","content":[{"type":"text","text":"reply"}]},
	  {"role":"user","content":"second"}]}`
	if rec := post(t, p, body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(fake.path("frames.jsonl"))
	if err != nil {
		t.Fatalf("frames were not recorded: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 frames, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], `"shouldQuery":false`) {
		t.Fatalf("historical user frame must not query: %s", lines[0])
	}
	if strings.Contains(lines[1], "shouldQuery") {
		t.Fatalf("assistant frame must not carry shouldQuery: %s", lines[1])
	}
	if strings.Contains(lines[2], "shouldQuery") {
		t.Fatalf("querying frame must omit shouldQuery: %s", lines[2])
	}
}

// One client request must reach the upstream exactly once, even when the CLI
// tries a second generation.
func TestClaudeCLIAdmitsOneUpstreamRequest(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "text", "upstream_calls": 3})
	if rec := post(t, p, baseBody); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := upstream.count(); got != 1 {
		t.Fatalf("expected exactly 1 upstream request, got %d", got)
	}
}

// Native's identity headers reach the upstream untouched: the subscription is
// authenticated by the CLI's own beta capability, not by a key aiproxy invents.
func TestClaudeCLIForwardsNativeHeaders(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "text", "upstream_calls": 1})
	if rec := post(t, p, baseBody); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.headers) == 0 {
		t.Fatal("upstream never saw a request")
	}
	if got := upstream.headers[0].Get("anthropic-beta"); !strings.Contains(got, "oauth-2025-04-20") {
		t.Fatalf("OAuth capability was not forwarded: %q", got)
	}
}

func TestClaudeCLIRejectsAssistantPrefill(t *testing.T) {
	p, _ := testProvider(t, "http://127.0.0.1:1", map[string]any{"mode": "text"})
	body := `{"model":"claude-sonnet-5-5","max_tokens":8,"messages":[
	  {"role":"user","content":"hi"},{"role":"assistant","content":"pre"}]}`
	rec := post(t, p, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestClaudeCLIRejectsInvalidToolName(t *testing.T) {
	p, _ := testProvider(t, "http://127.0.0.1:1", map[string]any{"mode": "text"})
	body := strings.Replace(baseBody, `"max_tokens":64`,
		`"tools":[{"name":"bad name!","input_schema":{"type":"object"}}],"max_tokens":64`, 1)
	rec := post(t, p, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A missing login is a 401 with the operator instruction, not a crash.
func TestClaudeCLIMissingLogin(t *testing.T) {
	fake := newFakeClaude(t, map[string]any{"mode": "text"})
	p := &ProviderConfig{
		ProviderID: "claude-test", Type: "claude-cli", Auth: "oauth", Enabled: true,
		BaseURL: "https://api.anthropic.com", ModelPrefix: "claude-test",
		Command: fake.bin, ClaudeConfigDir: filepath.Join(fake.dir, "absent"),
	}
	if err := p.initHTTPClient(); err != nil {
		t.Fatal(err)
	}
	rec := post(t, p, baseBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "docker compose run") {
		t.Fatalf("login hint missing: %s", rec.Body.String())
	}
}

// Garbage on stdout must be reported with the offending line, not swallowed.
func TestClaudeCLIReportsInvalidOutput(t *testing.T) {
	p, _ := testProvider(t, "http://127.0.0.1:1", map[string]any{"mode": "garbage"})
	rec := post(t, p, baseBody)
	// Nothing reached the client, so the headers were never committed: the failure
	// keeps a real status instead of a 200 whose stream mentions it. A gateway
	// failure is the class the host retries.
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d, want a gateway failure the host can retry: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stream-json") {
		t.Fatalf("diagnosis lost: %s", rec.Body.String())
	}
}

// A CLI that exits without answering is a failure, not an empty success.
func TestClaudeCLISilentExitFails(t *testing.T) {
	p, _ := testProvider(t, "http://127.0.0.1:1", map[string]any{"mode": "silent"})
	rec := post(t, p, baseBody)
	if rec.Code < 400 {
		t.Fatalf("a silent exit must fail with a real status, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatalf("diagnosis lost: %s", rec.Body.String())
	}
}

// An upstream refusal that lands before any content must reach the host as its
// own status, not as a 200 whose stream reports the failure in prose. The status
// is what the host's retry policy acts on: an overloaded upstream is retried,
// while the same words inside a 200 read as a terminal failure and the turn
// stops dead.
func TestClaudeCLIUpstreamRefusalKeepsItsStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Server is temporarily limiting requests (not your usage limit)"}}`))
	}))
	defer upstream.Close()

	p, _ := testProvider(t, upstream.URL, map[string]any{
		"mode": "text", "upstream_calls": 1, "report_upstream_error": true})
	rec := post(t, p, baseBody)

	if rec.Code != 529 {
		t.Fatalf("status %d, want the upstream's 529: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "event: message_start") {
		t.Fatalf("a refused generation must not open a stream: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "temporarily limiting") {
		t.Fatalf("upstream's own words lost: %s", rec.Body.String())
	}
}

// A 200 the upstream cuts short has no status of its own to report, so it becomes
// a gateway failure — the retryable class — rather than a committed 200 that the
// host cannot tell apart from an empty answer.
func TestClaudeCLITruncatedStreamBecomesGatewayFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"cut\",\"role\":\"assistant\",\"content\":[]}}\n\n"))
	}))
	defer upstream.Close()

	p, _ := testProvider(t, upstream.URL, map[string]any{
		"mode": "text", "upstream_calls": 1, "report_upstream_error": true})
	rec := post(t, p, baseBody)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want a gateway failure: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "incomplete") {
		t.Fatalf("the cut stream must be described: %s", rec.Body.String())
	}
}

// Once content has reached the client the 200 is committed and the status is gone,
// so the terminal error frame is the only signal left. It must carry the machine
// cause and must not quote the upstream: "not your usage limit" inside a denial is
// read as a billing verdict, and the turn stops instead of retrying.
func TestClaudeCLIMidStreamAbortDoesNotQuoteTheUpstream(t *testing.T) {
	p, _ := testProvider(t, "http://127.0.0.1:1", map[string]any{"mode": "drop_midstream"})
	rec := post(t, p, baseBody)
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the committed 200: %s", rec.Code, body)
	}
	if !strings.Contains(body, "event: error") {
		t.Fatalf("the abort must end the stream: %s", body)
	}
	if strings.Contains(body, "usage limit") {
		t.Fatalf("the upstream's words leaked into the error frame: %s", body)
	}
	if !strings.Contains(body, "upstream_status_529") {
		t.Fatalf("the machine cause is missing: %s", body)
	}
	if !strings.Contains(body, `"status":529`) {
		t.Fatalf("the error frame must carry the status: %s", body)
	}
}

func writeFakeConfig(t *testing.T, fake *fakeClaude, cfg map[string]any) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake.path("fake.conf.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A native auth failure must become 401 with the login instruction — not an
// empty 200 and not a generic 500. The shape asserted here is verbatim from
// Claude Code 2.1.311: the code rides the event, the result still says success.
func TestClaudeCLIAuthFailureBecomes401(t *testing.T) {
	p, _ := testProvider(t, "http://127.0.0.1:1", map[string]any{"mode": "auth_error"})
	rec := post(t, p, strings.Replace(baseBody, `"stream":true`, `"stream":false`, 1))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "docker compose run") {
		t.Fatalf("login instruction missing: %s", rec.Body.String())
	}
}

// The same failure inside a stream must still end with an error event.
func TestClaudeCLIAuthFailureInStream(t *testing.T) {
	p, _ := testProvider(t, "http://127.0.0.1:1", map[string]any{"mode": "auth_error"})
	rec := post(t, p, baseBody)
	// An auth refusal is answered before any content, so it reaches the host as
	// its own status — 401 — instead of a 200 carrying the words in a stream.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "docker compose run") {
		t.Fatalf("login instruction missing: %s", rec.Body.String())
	}
}

// A provider that declares exactly one model must still be listed: the merge
// helper used to emit a bare object for that case, which failed to unmarshal.
func TestModelsListIncludesSingleModelProvider(t *testing.T) {
	p := ProviderConfig{
		ProviderID: "single", Type: "claude-cli", Auth: "oauth", Enabled: true,
		BaseURL: "https://api.anthropic.com", ModelPrefix: "single",
		Models: []json.RawMessage{json.RawMessage(`{"id":"only-one","name":"Only"}`)},
	}
	models, err := FetchModels(t.Context(), p)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "only-one" {
		t.Fatalf("single declared model was dropped: %+v", models)
	}
}

// The transport must call the provider's own /v1/messages exactly once. The
// relay used to append the path to a URL that already ended in it, so a live run
// hit /v1/messages/v1/messages and every real request would 404.
func TestClaudeCLIUpstreamPathIsNotDoubled(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "text", "upstream_calls": 1})
	rec := post(t, p, baseBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	upstream.mu.Lock()
	paths := append([]string(nil), upstream.paths...)
	upstream.mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("upstream was never called")
	}
	for _, path := range paths {
		if path != "/v1/messages" {
			t.Fatalf("upstream path %q, want /v1/messages", path)
		}
	}
}

// A compressed upstream answer must reach the CLI decoded. The relay strips the
// client's Accept-Encoding so Go negotiates and decompresses; when it forwarded
// the header instead, the CLI received gzip bytes labelled as plain text.
func TestClaudeCLIGzippedUpstream(t *testing.T) {
	upstream := &upstreamRecorder{gzip: true}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "text"})
	rec := post(t, p, baseBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("stream did not decode:\n%s", body)
	}
	if strings.ContainsRune(body, 0x1f) {
		t.Fatalf("response still carries compressed bytes:\n%q", body)
	}
}

// The child must get the provider's own proxy and never inherit the container's.
// Without this, a host-wide HTTP_PROXY sends the loopback relay hop through the
// proxy and every request dies with ECONNREFUSED.
func TestClaudeCLIChildGetsProviderProxyAndBypassesLoopback(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, fake := testProvider(t, server.URL, map[string]any{"mode": "text", "dump_env": ""})
	dump := fake.path("env.json")
	// Rewrite the fake's config with the dump target.
	cfg, _ := json.Marshal(map[string]any{"mode": "text", "dump_env": dump})
	if err := os.WriteFile(fake.path("fake.conf.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	p.ProxyHost = "10.20.30.40:3128"
	// A container-wide proxy must not reach the child.
	t.Setenv("HTTP_PROXY", "http://wrong.example:1")
	t.Setenv("https_proxy", "http://wrong.example:1")

	rec := post(t, p, baseBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("child env was not captured: %v", err)
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if got := env[key]; got != "http://10.20.30.40:3128" {
			t.Fatalf("%s = %q, want the provider's proxy", key, got)
		}
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		got := env[key]
		if !strings.Contains(got, "127.0.0.1") || !strings.Contains(got, "localhost") {
			t.Fatalf("%s = %q, must cover loopback", key, got)
		}
	}
	// The subscription credential must never travel as an env var, and the host's
	// HOME must never be reused.
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := env[forbidden]; ok {
			t.Fatalf("%s leaked into the child environment", forbidden)
		}
	}
	if home := env["HOME"]; home == os.Getenv("HOME") {
		t.Fatalf("child inherited the host HOME %q", home)
	}
}

// A provider without proxy_host must not hand the child any proxy at all.
func TestClaudeCLIChildHasNoProxyWhenProviderHasNone(t *testing.T) {
	if got := childProxyURL(""); got != "" {
		t.Fatalf("childProxyURL(\"\") = %q, want empty", got)
	}
	if got := childProxyURL("10.20.30.40:3128"); got != "http://10.20.30.40:3128" {
		t.Fatalf("host:port normalisation = %q", got)
	}
	// A scheme the CLI cannot use must not be handed over as if it worked.
	if got := childProxyURL("socks5://10.20.30.40:1080"); got != "" {
		t.Fatalf("socks5 must be refused, got %q", got)
	}
}

// The signature_delta is what makes a thinking block replayable. Dropping it
// looked fine in a single turn and silently lost multi-turn reasoning.
func TestClaudeCLIForwardsThinkingSignature(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "text"})
	rec := post(t, p, baseBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "signature_delta") || !strings.Contains(body, "sig-1") {
		t.Fatalf("thinking signature was not forwarded to the client:\n%s", body)
	}
}

// Reasoning, then answer, then the call: the order a client replays.
func TestClaudeCLIBlockOrderThinkingTextTool(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "tool"})
	body := strings.Replace(baseBody, `"max_tokens":64`,
		`"tools":[{"name":"read_file","description":"read","input_schema":{"type":"object"}}],"max_tokens":64`, 1)
	body = strings.Replace(body, `"stream":true`, `"stream":false`, 1)
	rec := post(t, p, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var response struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, block := range response.Content {
		order = append(order, block.Type)
	}
	want := []string{"thinking", "text", "tool_use"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("block order %v, want %v", order, want)
	}
}

// ReadIdleSeconds bounds silence, not the whole turn: a slow but steady stream
// must survive far longer than the idle gap. A total deadline used to kill it.
func TestClaudeCLIIdleTimeoutIsAGapNotABudget(t *testing.T) {
	upstream := &upstreamRecorder{delay: 120 * time.Millisecond}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "text", "upstream_calls": 1})
	p.ReadIdleSeconds = 1 // one second of silence allowed

	start := time.Now()
	rec := post(t, p, baseBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("test finished in %s, the delay never applied", elapsed)
	}
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatalf("stream was cut:\n%s", rec.Body.String())
	}
}

// proxyRecorder is an HTTP proxy that answers generation requests itself and
// records what it was asked to reach.
type proxyRecorder struct {
	mu    sync.Mutex
	hits  int
	hosts []string
}

func (p *proxyRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hits++
		p.hosts = append(p.hosts, r.URL.Host)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
}

func (p *proxyRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

// Each provider must egress through its OWN proxy. A single shared proxy (or a
// process-wide one) would silently route one provider's traffic through another
// provider's egress, which is the whole point of configuring it per provider.
func TestPerProviderProxyIsolation(t *testing.T) {
	proxyA := &proxyRecorder{}
	serverA := httptest.NewServer(proxyA.handler())
	defer serverA.Close()
	proxyB := &proxyRecorder{}
	serverB := httptest.NewServer(proxyB.handler())
	defer serverB.Close()

	// Unresolvable on purpose: reaching it directly instead of via the proxy would
	// fail loudly rather than silently pass.
	pA, _ := testProvider(t, "http://upstream-a.invalid", map[string]any{"mode": "text", "upstream_calls": 1})
	pA.BaseURL = "http://upstream-a.invalid"
	pA.ProxyHost = serverA.URL
	if err := pA.initHTTPClient(); err != nil {
		t.Fatal(err)
	}

	pB, _ := testProvider(t, "http://upstream-b.invalid", map[string]any{"mode": "text", "upstream_calls": 1})
	pB.BaseURL = "http://upstream-b.invalid"
	pB.ProxyHost = serverB.URL
	if err := pB.initHTTPClient(); err != nil {
		t.Fatal(err)
	}

	if rec := post(t, pA, baseBody); rec.Code != http.StatusOK {
		t.Fatalf("provider A: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := proxyA.count(); got != 1 {
		t.Fatalf("proxy A saw %d requests, want 1", got)
	}
	if got := proxyB.count(); got != 0 {
		t.Fatalf("proxy B saw %d requests after a provider-A call, want 0", got)
	}

	if rec := post(t, pB, baseBody); rec.Code != http.StatusOK {
		t.Fatalf("provider B: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := proxyB.count(); got != 1 {
		t.Fatalf("proxy B saw %d requests, want 1", got)
	}

	proxyA.mu.Lock()
	hostsA := append([]string(nil), proxyA.hosts...)
	proxyA.mu.Unlock()
	proxyB.mu.Lock()
	hostsB := append([]string(nil), proxyB.hosts...)
	proxyB.mu.Unlock()
	if len(hostsA) == 0 || hostsA[0] != "upstream-a.invalid" {
		t.Fatalf("proxy A was asked for %v, want upstream-a.invalid", hostsA)
	}
	if len(hostsB) == 0 || hostsB[0] != "upstream-b.invalid" {
		t.Fatalf("proxy B was asked for %v, want upstream-b.invalid", hostsB)
	}
}

// A dead proxy must fail the request; falling back to a direct connection would
// quietly send subscription traffic outside the isolated channel.
func TestClaudeCLIDeadProxyNeverFallsBack(t *testing.T) {
	reached := 0
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		upstream.handler().ServeHTTP(w, r)
	}))
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{
		"mode": "text", "upstream_calls": 1, "fail_without_upstream": true})
	p.ProxyHost = "127.0.0.1:1" // nothing listens here
	if err := p.initHTTPClient(); err != nil {
		t.Fatal(err)
	}

	rec := post(t, p, baseBody)
	body := rec.Body.String()
	// Nothing reached the client, so the failure carries a real status instead of
	// a 200 whose stream reports it — the host acts on the status. The CLI renders
	// its own synthetic answer on an upstream failure, and those deltas must not
	// reach the client as if the generation had happened.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("dead proxy did not fail the turn with a gateway status: %d %s", rec.Code, body)
	}
	if strings.Contains(body, "Hello world") {
		t.Fatalf("the CLI's synthetic answer leaked to the client: %s", body)
	}
	if !strings.Contains(strings.ToLower(body), "connection refused") {
		t.Fatalf("diagnosis lost: %s", body)
	}
	if reached != 0 {
		t.Fatalf("upstream was reached %d times despite the dead proxy", reached)
	}

	// The non-streaming path can still report a real status.
	nonStream := strings.Replace(baseBody, `"stream":true`, `"stream":false`, 1)
	if rec := post(t, p, nonStream); rec.Code != http.StatusBadGateway && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-streaming status %d, want a gateway failure: %s", rec.Code, rec.Body.String())
	}
}
