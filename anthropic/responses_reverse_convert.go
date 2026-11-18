package anthropic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/whtsky/copilot2api/internal/types"
)

// Native Go converter for the OpenAI Responses API <-> Anthropic Messages API,
// in the direction "client speaks Responses, upstream speaks Messages"
// (Codex -> DeepSeek / Z.ai / Anthropic / Claude via Copilot). It is the mirror
// of ConvertAnthropicToResponses.
//
// The converters work on generic JSON (map[string]any with json.Number) rather
// than on the typed request structs: Responses requests carry many optional and
// polymorphic fields (string-or-array input, namespace tools, files...) and
// nothing may be lost silently.
//
// Reference behaviour is pinned by the committed fixtures in
// anthropic/testdata/reference; intentional deviations are documented in
// anthropic/testdata/reference/DIFFERENCES.md.

const (
	// defaultResponsesMaxTokens is used when the client sent no
	// max_output_tokens (Anthropic requires max_tokens).
	defaultResponsesMaxTokens = 32000
	// namespaceSeparator joins a Responses tool namespace and tool name into one
	// Anthropic tool name ("mcp__srv__" + "--" + "get" = "mcp__srv__--get").
	namespaceSeparator = "--"
	// redactedMarker prefixes encrypted_content of reasoning items that came from
	// an Anthropic redacted_thinking block, so they can be told apart from signed
	// thinking blocks with empty text when the history is sent back.
	redactedMarker = "aiproxy:redacted:"
)

// ConvertResponsesRequestToAnthropic converts a Responses API request body to an
// Anthropic Messages request body. The returned warnings list fields that were
// dropped or changed (for debug logging); they are not errors.
func ConvertResponsesRequestToAnthropic(body []byte) ([]byte, []string, error) {
	v, err := decodeJSON(body)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid Responses JSON: %w", err)
	}
	req, ok := v.(map[string]any)
	if !ok {
		return nil, nil, errors.New("invalid Responses request: JSON object expected")
	}
	if _, has := req["input"]; !has {
		return nil, nil, errors.New("invalid Responses request: missing field `input`")
	}

	c := &reqConv{toolNames: map[string]bool{}}
	out := map[string]any{"model": str(req["model"])}

	maxTokens := defaultResponsesMaxTokens
	if raw, present := req["max_output_tokens"]; present && raw != nil {
		n, ok := toInt(raw)
		if !ok || n < 1 {
			return nil, nil, errors.New("max_output_tokens must be at least 1")
		}
		maxTokens = n
	}
	out["max_tokens"] = maxTokens
	out["cache_control"] = map[string]any{"type": "ephemeral"}

	// Tools first: function_call names in the history depend on them.
	tools := c.convertTools(req["tools"])
	if len(tools) > 0 {
		out["tools"] = tools
	}

	if s := str(req["instructions"]); strings.TrimSpace(s) != "" {
		c.system = append(c.system, s)
	}
	c.convertInput(req["input"])

	if len(c.system) == 1 {
		out["system"] = c.system[0]
	} else if len(c.system) > 1 {
		blocks := make([]any, 0, len(c.system))
		for _, s := range c.system {
			blocks = append(blocks, map[string]any{"type": "text", "text": s})
		}
		out["system"] = blocks
	}
	out["messages"] = c.msgs.marshal()

	if tc := c.convertToolChoice(req["tool_choice"], req["parallel_tool_calls"], len(tools) > 0); tc != nil {
		out["tool_choice"] = tc
	}
	c.applyReasoning(req["reasoning"], out)
	c.applyTextFormat(req["text"], out)

	if t, ok := req["temperature"]; ok && t != nil {
		out["temperature"] = t
	}
	if t, ok := req["top_p"]; ok && t != nil {
		out["top_p"] = t
	}
	if b, ok := req["stream"].(bool); ok && b {
		out["stream"] = true
	}

	// metadata: Anthropic only knows user_id.
	userID := ""
	if md, ok := req["metadata"].(map[string]any); ok {
		userID = str(md["user_id"])
	}
	if userID == "" {
		userID = str(req["user"])
	}
	if userID != "" {
		out["metadata"] = map[string]any{"user_id": userID}
	}
	if s := str(req["previous_response_id"]); s != "" {
		c.warn("previous_response_id dropped (stateless proxy: send the full history)")
	}

	b, err := marshalNoEscape(out)
	if err != nil {
		return nil, nil, err
	}
	return b, c.warnings, nil
}

// ---------------------------------------------------------------------------

type reqConv struct {
	system    []string
	msgs      msgList
	toolNames map[string]bool
	warnings  []string
}

func (c *reqConv) warn(s string) { c.warnings = append(c.warnings, s) }

type outMsg struct {
	role   string
	blocks []map[string]any
	plain  bool // content was a single plain string in the source
}

type msgList struct{ msgs []*outMsg }

// add appends blocks as a message of the given role, merging with the previous
// message when the role is the same (Anthropic requires alternating roles).
func (l *msgList) add(role string, blocks []map[string]any, plain bool) {
	if len(blocks) == 0 {
		return
	}
	if n := len(l.msgs); n > 0 && l.msgs[n-1].role == role {
		last := l.msgs[n-1]
		last.blocks = append(last.blocks, blocks...)
		last.plain = false
		return
	}
	l.msgs = append(l.msgs, &outMsg{role: role, blocks: blocks, plain: plain})
}

func (l *msgList) marshal() []any {
	res := make([]any, 0, len(l.msgs))
	for _, m := range l.msgs {
		var content any
		if m.plain && len(m.blocks) == 1 && m.blocks[0]["type"] == "text" {
			content = m.blocks[0]["text"]
		} else {
			content = m.blocks
		}
		res = append(res, map[string]any{"role": m.role, "content": content})
	}
	return res
}

func (c *reqConv) convertInput(input any) {
	switch in := input.(type) {
	case string:
		if in != "" {
			c.msgs.add("user", []map[string]any{{"type": "text", "text": in}}, true)
		}
	case []any:
		for _, raw := range in {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ := str(item["type"])
			if typ == "" && item["role"] != nil {
				typ = "message"
			}
			switch typ {
			case "message":
				c.convertMessage(item)
			case "reasoning":
				c.convertReasoning(item)
			case "function_call":
				c.convertFunctionCall(item)
			case "function_call_output":
				c.convertFunctionOutput(item)
			default:
				c.warn("input item dropped: type " + typ)
			}
		}
	}
}

func (c *reqConv) convertMessage(item map[string]any) {
	role := str(item["role"])
	blocks, plain := c.contentBlocks(item["content"], role)
	switch role {
	case "system", "developer":
		var parts []string
		for _, b := range blocks {
			if b["type"] == "text" {
				parts = append(parts, str(b["text"]))
			}
		}
		if s := strings.Join(parts, "\n"); strings.TrimSpace(s) != "" {
			c.system = append(c.system, s)
		}
	case "assistant":
		c.msgs.add("assistant", blocks, plain)
	default: // user and anything unexpected
		c.msgs.add("user", blocks, plain)
	}
}

// contentBlocks converts Responses message content (string or part array).
// Empty text is skipped: the Anthropic API rejects empty text blocks.
func (c *reqConv) contentBlocks(content any, role string) ([]map[string]any, bool) {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil, false
		}
		return []map[string]any{{"type": "text", "text": v}}, true
	case []any:
		var blocks []map[string]any
		for _, raw := range v {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch typ := str(part["type"]); typ {
			case "input_text", "output_text", "text":
				if t := str(part["text"]); t != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": t})
				}
			case "refusal":
				if t := str(part["refusal"]); t != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": t})
				}
			case "input_image":
				if role == "assistant" {
					continue
				}
				if b := imageBlock(part["image_url"]); b != nil {
					blocks = append(blocks, b)
				} else {
					c.warn("input_image dropped (no usable image_url; file_id is not supported)")
				}
			case "input_file":
				if role == "assistant" {
					continue
				}
				if b := fileBlock(part); b != nil {
					blocks = append(blocks, b)
				} else {
					c.warn("input_file dropped (only file_data and file_url are supported)")
				}
			default:
				c.warn("content part dropped: type " + typ)
			}
		}
		return blocks, false
	}
	return nil, false
}

func imageBlock(v any) map[string]any {
	url := ""
	switch u := v.(type) {
	case string:
		url = u
	case map[string]any:
		url = str(u["url"])
	}
	if url == "" {
		return nil
	}
	if mime, data, ok := parseDataURL(url); ok {
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": mime, "data": data}}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
}

func fileBlock(part map[string]any) map[string]any {
	var block map[string]any
	if fd := str(part["file_data"]); fd != "" {
		mime, data, ok := parseDataURL(fd)
		if !ok {
			mime, data = "application/pdf", fd
		}
		block = map[string]any{"type": "document", "source": map[string]any{
			"type": "base64", "media_type": mime, "data": data}}
	} else if u := str(part["file_url"]); u != "" {
		block = map[string]any{"type": "document", "source": map[string]any{"type": "url", "url": u}}
	} else {
		return nil
	}
	if name := str(part["filename"]); name != "" {
		block["title"] = name
	}
	return block
}

// parseDataURL splits "data:<mime>;base64,<data>".
func parseDataURL(s string) (mime, data string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	comma := strings.Index(s, ",")
	if comma < 0 {
		return "", "", false
	}
	meta := s[len("data:"):comma]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mime = strings.TrimSuffix(meta, ";base64")
	if mime == "" {
		mime = "application/octet-stream"
	}
	return mime, s[comma+1:], true
}

func (c *reqConv) convertReasoning(item map[string]any) {
	enc := str(item["encrypted_content"])
	if enc == "" {
		c.warn("reasoning item dropped (no encrypted_content: cannot be replayed to Anthropic)")
		return
	}
	var texts []string
	if sum, ok := item["summary"].([]any); ok {
		for _, s := range sum {
			if m, ok := s.(map[string]any); ok {
				if t := str(m["text"]); t != "" {
					texts = append(texts, t)
				}
			}
		}
	}
	// Only blocks that really were redacted_thinking (marked on the way out, see
	// redactedMarker) go back as redacted_thinking. Everything else is a normal
	// thinking block, even with empty text: providers such as DeepSeek return
	// signed thinking blocks with empty text and reject redacted_thinking.
	var block map[string]any
	if data, redacted := strings.CutPrefix(enc, redactedMarker); redacted {
		block = map[string]any{"type": "redacted_thinking", "data": data}
	} else {
		block = map[string]any{"type": "thinking", "thinking": strings.Join(texts, "\n"), "signature": enc}
	}
	c.msgs.add("assistant", []map[string]any{block}, false)
}

func (c *reqConv) convertFunctionCall(item map[string]any) {
	name := str(item["name"])
	if ns := str(item["namespace"]); ns != "" {
		// Use the namespaced name only if that is how the tool was declared;
		// flat tools (e.g. expanded by the DeepSeek adapter) keep their name.
		if full := ns + namespaceSeparator + name; c.toolNames[full] {
			name = full
		}
	}
	input := map[string]any{}
	switch args := str(item["arguments"]); {
	case strings.TrimSpace(args) == "":
		c.warn("function_call with empty arguments: sent as {}")
	default:
		v, err := decodeJSON([]byte(args))
		if m, ok := v.(map[string]any); err == nil && ok {
			input = m
		} else {
			c.warn("function_call arguments are not a JSON object: sent as {}")
		}
	}
	c.msgs.add("assistant", []map[string]any{{
		"type": "tool_use", "id": str(item["call_id"]), "name": name, "input": input}}, false)
}

func (c *reqConv) convertFunctionOutput(item map[string]any) {
	var content any
	switch out := item["output"].(type) {
	case nil:
		content = ""
	case string:
		content = out
	case []any:
		blocks, _ := c.contentBlocks(out, "user")
		if len(blocks) == 0 {
			content = ""
		} else {
			content = blocks
		}
	default:
		b, _ := marshalNoEscape(out)
		content = string(b)
	}
	c.msgs.add("user", []map[string]any{{
		"type": "tool_result", "tool_use_id": str(item["call_id"]), "content": content}}, false)
}

// ---------------------------------------------------------------------------
// tools

func (c *reqConv) convertTools(v any) []map[string]any {
	list, _ := v.([]any)
	var out []map[string]any
	for _, raw := range list {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := str(t["type"])
		switch {
		case typ == "function" || typ == "":
			if tool := c.functionTool(t, "", ""); tool != nil {
				out = append(out, tool)
			}
		case typ == "namespace":
			ns := str(t["name"])
			nsDesc := str(t["description"])
			sub, _ := t["tools"].([]any)
			for _, s := range sub {
				st, ok := s.(map[string]any)
				if !ok {
					continue
				}
				if tool := c.functionTool(st, ns, nsDesc); tool != nil {
					out = append(out, tool)
				}
			}
		case strings.HasPrefix(typ, "web_search"):
			tool := map[string]any{"type": "web_search_20260209", "name": "web_search"}
			domains := t["allowed_domains"]
			if f, ok := t["filters"].(map[string]any); ok && domains == nil {
				domains = f["allowed_domains"]
			}
			if domains != nil {
				tool["allowed_domains"] = domains
			}
			if loc, ok := t["user_location"].(map[string]any); ok {
				ul := map[string]any{"type": "approximate"}
				for _, k := range []string{"city", "country", "region", "timezone"} {
					if s := str(loc[k]); s != "" {
						ul[k] = s
					}
				}
				tool["user_location"] = ul
			}
			out = append(out, tool)
		case typ == "tool_search":
			out = append(out, map[string]any{"name": "tool_search_tool_bm25", "type": "tool_search_tool_bm25_20251119"})
		default:
			c.warn("tool dropped: unsupported type " + typ)
		}
	}
	return out
}

// functionTool converts one function tool; ns != "" prefixes the name.
func (c *reqConv) functionTool(t map[string]any, ns, nsDesc string) map[string]any {
	src := t
	if fn, ok := t["function"].(map[string]any); ok { // Chat-Completions-style nesting
		src = fn
	}
	name := str(src["name"])
	if name == "" {
		name = str(t["name"])
	}
	if name == "" {
		c.warn("function tool dropped: no name")
		return nil
	}
	if ns != "" {
		name = ns + namespaceSeparator + name
	}
	tool := map[string]any{"name": name}
	desc := str(src["description"])
	if desc == "" {
		desc = nsDesc
	}
	if desc != "" {
		tool["description"] = desc
	}
	params := src["parameters"]
	if params == nil {
		params = t["parameters"]
	}
	if params == nil {
		params = map[string]any{"type": "object"}
	}
	tool["input_schema"] = params
	if s, ok := t["strict"].(bool); ok {
		tool["strict"] = s
	}
	c.toolNames[name] = true
	return tool
}

func (c *reqConv) convertToolChoice(tc, parallel any, hasTools bool) map[string]any {
	if !hasTools {
		return nil
	}
	var out map[string]any
	switch v := tc.(type) {
	case string:
		out = toolChoiceFromString(v)
	case map[string]any:
		switch typ := str(v["type"]); typ {
		case "function", "tool":
			name := str(v["name"])
			if name == "" {
				if fn, ok := v["function"].(map[string]any); ok {
					name = str(fn["name"])
				}
			}
			if name != "" {
				if ns := str(v["namespace"]); ns != "" && c.toolNames[ns+namespaceSeparator+name] {
					name = ns + namespaceSeparator + name
				}
				out = map[string]any{"type": "tool", "name": name}
			}
		case "auto", "required", "none":
			out = toolChoiceFromString(typ)
		default:
			c.warn("tool_choice dropped: unsupported type " + typ)
		}
	}
	if b, ok := parallel.(bool); ok && !b {
		if out == nil {
			out = map[string]any{"type": "auto"}
		}
		if out["type"] != "none" {
			out["disable_parallel_tool_use"] = true
		}
	}
	return out
}

func toolChoiceFromString(s string) map[string]any {
	switch s {
	case "auto":
		return map[string]any{"type": "auto"}
	case "required":
		return map[string]any{"type": "any"}
	case "none":
		return map[string]any{"type": "none"}
	}
	return nil
}

// ---------------------------------------------------------------------------
// reasoning / structured output

func (c *reqConv) applyReasoning(v any, out map[string]any) {
	r, ok := v.(map[string]any)
	if !ok {
		return
	}
	effort := str(r["effort"])
	summary := str(r["summary"])
	switch effort {
	case "none":
		out["thinking"] = map[string]any{"type": "disabled"}
		return
	case "minimal":
		c.warn("reasoning.effort minimal changed to low")
		effort = "low"
	}
	if effort != "" {
		oc := outputConfig(out)
		oc["effort"] = effort
	}
	if effort != "" || summary != "" {
		display := "omitted"
		switch summary {
		case "auto", "concise", "detailed":
			display = "summarized"
		}
		out["thinking"] = map[string]any{"type": "adaptive", "display": display}
	}
}

func (c *reqConv) applyTextFormat(v any, out map[string]any) {
	t, ok := v.(map[string]any)
	if !ok {
		return
	}
	f, ok := t["format"].(map[string]any)
	if !ok {
		return
	}
	switch typ := str(f["type"]); typ {
	case "json_schema":
		schema := f["schema"]
		if schema == nil {
			if js, ok := f["json_schema"].(map[string]any); ok {
				schema = js["schema"]
			}
		}
		if schema != nil {
			outputConfig(out)["format"] = map[string]any{"type": "json_schema", "schema": schema}
		}
	case "json_object":
		c.warn("text.format json_object dropped (no Anthropic equivalent)")
	}
}

func outputConfig(out map[string]any) map[string]any {
	if oc, ok := out["output_config"].(map[string]any); ok {
		return oc
	}
	oc := map[string]any{}
	out["output_config"] = oc
	return oc
}

// ---------------------------------------------------------------------------
// shared helpers

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			f, ferr := n.Float64()
			if ferr != nil {
				return 0, false
			}
			return int(f), true
		}
		return int(i), true
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// derivedID builds a deterministic Responses-style item id (prefix + 48 hex).
func derivedID(prefix, seed string, idx int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", prefix, seed, idx)))
	return prefix + hex.EncodeToString(h[:])[:48]
}

// splitToolNamespace undoes the "<namespace>--<tool>" naming used for
// namespaced tools, falling back to the MCP registry for flat tool names.
func splitToolNamespace(name string) (ns, short string) {
	if i := strings.Index(name, namespaceSeparator); i > 0 && i+len(namespaceSeparator) < len(name) {
		return name[:i], name[i+len(namespaceSeparator):]
	}
	return types.ResolveMcpToolNamespace(name), name
}
