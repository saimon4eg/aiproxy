package providers

// Anthropic /v1/messages request → Claude Code stream-json input frames.
//
// Hermes owns the agent loop, so the CLI must act as a pure model client: every
// native capability that would let it act on its own is disabled at spawn time
// (see claudecode_proc.go). This file only translates the request.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/whtsky/copilot2api/anthropic"
)

// toolPrefix is what native sees for every host tool. MCP names the model calls
// back are "mcp__hermes__<wire>", capped at 64 characters by the CLI.
const (
	toolPrefix      = "mcp__hermes__"
	maxWireNameLen  = 50 // 64 - len(toolPrefix) leaves 51; keep one spare
	shortHeadLen    = 41
	shortHashLen    = 8
	maxSystemLength = 4 * 1024 * 1024
)

// nativeFrame is one line of the CLI's stream-json input.
//
// shouldQuery is written only on historical frames, as `false`; the querying
// frame omits it entirely (that is the contract native expects).
type nativeFrame struct {
	Type        string         `json:"type"`
	Message     map[string]any `json:"message,omitempty"`
	ShouldQuery *bool          `json:"shouldQuery,omitempty"`
}

// claudeRequest is everything the spawn needs, derived from the client request.
type claudeRequest struct {
	System     string
	Frames     []nativeFrame
	ExtraBody  string            // CLAUDE_CODE_EXTRA_BODY contents
	Manifest   []manifestTool    // inert MCP inventory
	WireToHost map[string]string // shortened wire name → host tool name
	HostToWire map[string]string // host tool name → wire name
	MaxTokens  int
	Effort     string
	Model      string
	Stream     bool
}

type manifestTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// wireName shortens a host tool name that cannot fit the native MCP budget.
// The mapping is deterministic so replayed history produces the same wire name.
func wireName(name string) string {
	if len(name) <= maxWireNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:shortHeadLen] + "_" + hex.EncodeToString(sum[:])[:shortHashLen]
}

// buildClaudeRequest validates the client request and translates it. Errors are
// fail-closed: a shape we cannot faithfully replay must not reach the model.
func buildClaudeRequest(req *anthropic.AnthropicMessagesRequest) (*claudeRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("model is required")
	}
	if req.ToolChoice != nil && req.ToolChoice.Type != "" && req.ToolChoice.Type != "auto" {
		return nil, fmt.Errorf("only tool_choice=auto is supported")
	}
	if req.MaxTokens < 0 {
		return nil, fmt.Errorf("max_tokens must not be negative")
	}

	system, frames, err := buildFrames(req.Messages)
	if err != nil {
		return nil, err
	}
	// The top-level `system` field is where Anthropic-shaped clients (Hermes with
	// api_mode anthropic_messages) put the whole system prompt. It precedes any
	// `role: system` messages, exactly as upstream orders them. Dropping it left
	// the model with the CLI's one-line default prompt.
	system, err = withSystemField(req.System, system)
	if err != nil {
		return nil, err
	}

	plan := &claudeRequest{
		System:     system,
		Frames:     frames,
		MaxTokens:  req.MaxTokens,
		Model:      req.Model,
		Stream:     req.Stream,
		WireToHost: map[string]string{},
		HostToWire: map[string]string{},
	}
	if req.OutputConfig != nil {
		plan.Effort = req.OutputConfig.Effort
	}

	// Tool inventory: the inert MCP server and the request body must advertise the
	// same shape, so both are built from one pass.
	tools := make([]map[string]any, 0, len(req.Tools))
	seen := map[string]bool{}
	for _, tool := range req.Tools {
		if tool.Name == "" {
			return nil, fmt.Errorf("tool name must not be empty")
		}
		for _, r := range tool.Name {
			valid := r == '_' || r == '-' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !valid {
				return nil, fmt.Errorf("tool name %q must be ASCII letters, digits, _ or -", tool.Name)
			}
		}
		wire := wireName(tool.Name)
		if seen[wire] {
			return nil, fmt.Errorf("tool name %q collides with another tool after shortening", tool.Name)
		}
		seen[wire] = true
		plan.WireToHost[wire] = tool.Name
		plan.HostToWire[tool.Name] = wire

		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		plan.Manifest = append(plan.Manifest, manifestTool{
			Name: wire, Description: tool.Description, InputSchema: schema,
		})
		tools = append(tools, map[string]any{
			"name":         toolPrefix + wire,
			"description":  tool.Description,
			"input_schema": schema,
		})
	}

	body := map[string]any{}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if req.Thinking != nil && req.Thinking.Type != "" && req.Thinking.Type != "disabled" {
		thinking := map[string]any{"type": req.Thinking.Type}
		if req.Thinking.BudgetTokens != nil {
			thinking["budget_tokens"] = *req.Thinking.BudgetTokens
		}
		// "display" decides whether the reasoning text is returned at all: on
		// 4.7+ it defaults to "omitted", so dropping the client's choice turns
		// every thinking block into an empty, signature-only one and the client
		// shows no reasoning.
		if req.Thinking.Display != "" {
			thinking["display"] = req.Thinking.Display
		}
		body["thinking"] = thinking
	}
	if req.OutputConfig != nil {
		cfg := map[string]any{}
		if req.OutputConfig.Effort != "" {
			cfg["effort"] = req.OutputConfig.Effort
		}
		if len(cfg) > 0 {
			body["output_config"] = cfg
		}
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.StopSequences) > 0 {
		body["stop_sequences"] = req.StopSequences
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode native extra body: %w", err)
	}
	plan.ExtraBody = string(encoded)
	return plan, nil
}

// buildFrames converts Anthropic messages into the CLI's frame sequence.
//
// History must end in a non-empty user/tool-result frame: assistant prefill has
// no stream-json representation, and a trailing assistant turn would ask the
// model to continue its own sentence.
func buildFrames(messages []anthropic.AnthropicMessage) (string, []nativeFrame, error) {
	var system []string
	var frames []nativeFrame

	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			if len(frames) > 0 {
				return "", nil, fmt.Errorf("system messages must precede the conversation")
			}
			text, err := systemText(msg.Content)
			if err != nil {
				return "", nil, err
			}
			system = append(system, text)
			continue
		case "user", "assistant":
		case "tool":
			// Anthropic carries tool results as user blocks; accept a plain tool
			// role too so OpenAI-shaped histories survive the hop.
			blocks, err := toolRoleBlocks(msg.Content)
			if err != nil {
				return "", nil, err
			}
			frames = appendUserBlocks(frames, blocks)
			continue
		default:
			return "", nil, fmt.Errorf("unsupported message role %q", msg.Role)
		}

		blocks, err := contentBlocks(msg.Content)
		if err != nil {
			return "", nil, err
		}
		if msg.Role == "user" {
			frames = appendUserBlocks(frames, blocks)
			continue
		}
		frames = append(frames, nativeFrame{
			Type:    "assistant",
			Message: map[string]any{"role": "assistant", "content": blocks},
		})
	}

	if len(frames) == 0 {
		return "", nil, fmt.Errorf("at least one user message is required")
	}
	last := frames[len(frames)-1]
	if last.Type != "user" || emptyContent(last.Message["content"]) {
		return "", nil, fmt.Errorf("history must end with a non-empty user or tool-result message; assistant prefill is unsupported")
	}
	if len(system) == 0 {
		return "", frames, nil
	}
	joined := strings.Join(system, "\n\n")
	if len(joined) > maxSystemLength {
		return "", nil, fmt.Errorf("system prompt exceeds %d bytes", maxSystemLength)
	}
	return joined, frames, nil
}

// appendUserBlocks merges consecutive user frames: the CLI expects one user turn
// between assistant turns, and merging keeps replay byte-stable.
func appendUserBlocks(frames []nativeFrame, blocks []map[string]any) []nativeFrame {
	if len(frames) > 0 && frames[len(frames)-1].Type == "user" {
		existing, _ := frames[len(frames)-1].Message["content"].([]map[string]any)
		frames[len(frames)-1].Message["content"] = append(existing, blocks...)
		return frames
	}
	return append(frames, nativeFrame{
		Type:    "user",
		Message: map[string]any{"role": "user", "content": blocks},
	})
}

func emptyContent(v any) bool {
	switch c := v.(type) {
	case []map[string]any:
		return len(c) == 0
	case []any:
		return len(c) == 0
	case nil:
		return true
	}
	return false
}

// withSystemField puts the request's top-level `system` ahead of the text
// already gathered from `role: system` messages. The join is deterministic
// (blocks and parts separated by a blank line, empty parts skipped): the text
// lands in the CLI's system prompt, so any run-to-run difference would move the
// cached prefix.
func withSystemField(field *anthropic.AnthropicSystem, fromMessages string) (string, error) {
	var top string
	if field != nil {
		if field.Text != nil {
			top = *field.Text
		} else {
			var parts []string
			for _, block := range field.Blocks {
				if block.Type == "text" && block.Text != "" {
					parts = append(parts, block.Text)
				}
			}
			top = strings.Join(parts, "\n\n")
		}
	}
	joined := fromMessages
	switch {
	case top != "" && fromMessages != "":
		joined = top + "\n\n" + fromMessages
	case top != "":
		joined = top
	}
	if len(joined) > maxSystemLength {
		return "", fmt.Errorf("system prompt exceeds %d bytes", maxSystemLength)
	}
	return joined, nil
}

// systemText flattens an Anthropic system field into plain text.
func systemText(content anthropic.AnthropicContent) (string, error) {
	if content.Text != nil {
		return *content.Text, nil
	}
	var parts []string
	for _, block := range content.Blocks {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// toolRoleBlocks maps a bare tool message to Anthropic's tool_result shape.
func toolRoleBlocks(content anthropic.AnthropicContent) ([]map[string]any, error) {
	blocks, err := contentBlocks(content)
	if err != nil {
		return nil, err
	}
	for _, block := range blocks {
		if block["type"] != "tool_result" {
			return nil, fmt.Errorf("a tool message must carry tool_result blocks")
		}
	}
	return blocks, nil
}

// contentBlocks passes Anthropic content blocks through unchanged, dropping the
// fields native refuses (cache_control, citations) and images it cannot fetch.
func contentBlocks(content anthropic.AnthropicContent) ([]map[string]any, error) {
	if content.Text != nil {
		return []map[string]any{{"type": "text", "text": *content.Text}}, nil
	}
	blocks := make([]map[string]any, 0, len(content.Blocks))
	for _, raw := range content.Blocks {
		block, err := oneBlock(raw)
		if err != nil {
			return nil, err
		}
		if block != nil {
			blocks = append(blocks, block)
		}
	}
	return blocks, nil
}

func oneBlock(b anthropic.AnthropicContentBlock) (map[string]any, error) {
	switch b.Type {
	case "text":
		return map[string]any{"type": "text", "text": b.Text}, nil

	case "image":
		if b.Source == nil {
			return nil, fmt.Errorf("image block without a source")
		}
		if b.Source.Type != "base64" {
			// A remote URL would make the proxy fetch arbitrary network content on
			// the request path; hand the model a hint instead.
			return map[string]any{
				"type": "text",
				"text": "[image omitted: remote URLs are not fetched — call the host's vision tool instead]",
			}, nil
		}
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": b.Source.MediaType,
				"data":       b.Source.Data,
			},
		}, nil

	case "tool_use":
		input := b.Input
		if input == nil {
			input = map[string]any{}
		}
		return map[string]any{
			"type": "tool_use", "id": b.ID, "name": toolPrefix + wireName(b.Name), "input": input,
		}, nil

	case "tool_result":
		out := map[string]any{"type": "tool_result", "tool_use_id": b.ToolUseID}
		if b.Content != nil {
			if b.Content.Text != nil {
				out["content"] = *b.Content.Text
			} else {
				nested, err := contentBlocks(*b.Content)
				if err != nil {
					return nil, err
				}
				out["content"] = nested
			}
		}
		if b.IsError != nil {
			out["is_error"] = *b.IsError
		}
		return out, nil

	case "thinking":
		// Signed thinking only replays with its signature; without one the block
		// would be rejected, so drop it and let the model think again.
		if b.Signature == "" {
			return nil, nil
		}
		return map[string]any{
			"type": "thinking", "thinking": b.Thinking, "signature": b.Signature,
		}, nil

	case "redacted_thinking":
		// The block's payload has no field on our request type, so it cannot be
		// replayed faithfully. Dropping it is safe: native re-derives its own.
		return nil, nil

	case "":
		return nil, fmt.Errorf("content block without a type")
	default:
		// Unknown block types are dropped rather than forwarded: native rejects
		// shapes it does not know, and a rejection costs the whole turn.
		return nil, nil
	}
}

// hostToolName maps a wire name from the model back to the host's name.
func (p *claudeRequest) hostToolName(wire string) string {
	wire = strings.TrimPrefix(wire, toolPrefix)
	if host, ok := p.WireToHost[wire]; ok {
		return host
	}
	return wire
}

// offeredToolNames returns the wire names the model was given, sorted for stable logs.
func (p *claudeRequest) offeredToolNames() []string {
	names := make([]string, 0, len(p.WireToHost))
	for name := range p.WireToHost {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
