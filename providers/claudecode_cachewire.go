package providers

// Prompt-cache wire hygiene for the claude-cli transport.
//
// The official CLI decides where the message-level cache breakpoint goes, and it
// puts it on the per-request context it attaches to the turn it answers: today's
// date, the account e-mail reminder, whatever a later release adds. The next
// request replays that turn WITHOUT that context, so the cached prefix never
// recurs and every tool round re-writes the whole history at the cache-creation
// rate (1.25x) instead of reading it back (0.1x). Claude Code 2.1.287+ marks two
// message blocks from the second request on — the last tool_use of the turn and
// its trailing per-request `role: system` message — and that message never
// recurs either, so each round is written twice.
//
// The Python plugin that used to host this transport carried the fix inside its
// admission relay (pin_message_breakpoint / restore_queried_turn / _frame_first).
// The Go port of the relay forwards the CLI's body untouched, which is why the
// same subscription now draws several times faster per turn: shelling out per
// request is NOT the difference (the plugin did that too), the missing wire
// correction is.
//
// Three rules govern every function here:
//
//  1. Nothing is a gate. A body that cannot be parsed or reconciled forwards
//     exactly as the CLI built it — a wire change may cost a cache hit, never a
//     turn.
//  2. Content is never edited, only re-ordered and re-marked. Block bodies are
//     compared, not rewritten.
//  3. The breakpoint only ever moves EARLIER, onto content the next request
//     replays byte-identically, and message marks never multiply.

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

// errQueriedTurnMismatch means the CLI's body could not be proven losslessly
// reconcilable with the frame the client queried.
var errQueriedTurnMismatch = errors.New("native request does not uniquely match the queried tool results")

// uncacheable reports whether a block type must never carry a breakpoint:
// Anthropic rejects markers on reasoning blocks.
func uncacheable(blockType string) bool {
	return blockType == "thinking" || blockType == "redacted_thinking"
}

// decodeWireBody parses a request body keeping numbers as json.Number, so a
// re-encoded body does not silently round-trip through float64.
func decodeWireBody(payload []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	return body, nil
}

// encodeWireBody re-encodes compactly with HTML escaping off, matching what the
// plugin emitted: `<` and `&` must not become \u003c escapes inside tool output.
func encodeWireBody(body map[string]any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// plainBlock drops the cache directive so two blocks can be compared by content.
func plainBlock(value any) any {
	block, ok := value.(map[string]any)
	if !ok {
		return value
	}
	out := make(map[string]any, len(block))
	for key, item := range block {
		if key == "cache_control" {
			continue
		}
		out[key] = item
	}
	return out
}

// withoutKeys copies a map minus the named keys.
func withoutKeys(block map[string]any, keys ...string) map[string]any {
	drop := make(map[string]bool, len(keys))
	for _, key := range keys {
		drop[key] = true
	}
	out := make(map[string]any, len(block))
	for key, value := range block {
		if drop[key] {
			continue
		}
		out[key] = value
	}
	return out
}

// deepCopyValue copies the JSON shapes this file mutates.
func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = deepCopyValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = deepCopyValue(item)
		}
		return out
	default:
		return value
	}
}

// bareTextBlock reports whether the value is exactly `{type: text, text: str}`
// with no metadata and no cache directive — the only shape this file joins or
// splits across.
func bareTextBlock(value any) (string, bool) {
	block, ok := value.(map[string]any)
	if !ok || len(block) != 2 {
		return "", false
	}
	if kind, _ := block["type"].(string); kind != "text" {
		return "", false
	}
	text, ok := block["text"].(string)
	if !ok {
		return "", false
	}
	return text, true
}

// textOf flattens a string, or a run of bare text blocks, into one string.
func textOf(value any) (string, bool) {
	if text, ok := value.(string); ok {
		return text, true
	}
	blocks, ok := value.([]any)
	if !ok {
		return "", false
	}
	var builder strings.Builder
	for _, block := range blocks {
		text, ok := bareTextBlock(block)
		if !ok {
			return "", false
		}
		builder.WriteString(text)
	}
	return builder.String(), true
}

// wireBlockRef addresses one content block inside the messages array.
type wireBlockRef struct {
	message int
	index   int
	block   map[string]any
}

// before reports whether this block precedes the other in wire order.
func (r wireBlockRef) before(other wireBlockRef) bool {
	if r.message != other.message {
		return r.message < other.message
	}
	return r.index < other.index
}

// ---------------------------------------------------------------------------
// What this file does NOT do
//
// The native CLI injects its own per-request context into the request: a text
// block ahead of the first turn, a trailing `role: system` message, and an
// annotation appended to the last tool_result. It is tempting to strip that — it
// names the account, and a model can repeat it into the conversation — but the
// plugin this ports did NOT do that, and neither must this port: everything the
// CLI builds is forwarded as-is. The plugin's own tests pin exactly that (the
// prepended block reordered but kept; the trailing system message forwarded
// unchanged; only the queried tool-result frame restored). Restoring that frame
// is the sole content this file removes.
// ---------------------------------------------------------------------------

// frameFirst puts the client's first user turn ahead of the context native prepends
// to it.
//
// On the opening request native puts its per-request context block (the account-e-mail
// reminder) BEFORE the frame the client queried; on every later request that turn is
// replayed WITHOUT it. Nothing of the first turn therefore recurs, and call #2 re-writes
// the whole of it, which is most of a run whose first turn carries a source pack. Moving
// the prepended text blocks behind the frame uses the same order native uses on later
// turns: no block is added, dropped or edited, and the frame becomes a prefix the next
// request replays byte-identically.
//
// Only the conversation's first turn is touched (no assistant message yet), only when the
// queried frame occurs exactly once, and only when everything before it is plain text.
// Returns whether the order changed.
func frameFirst(messages []any, queried []any) bool {
	if len(queried) == 0 {
		return false
	}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role == "assistant" {
			return false
		}
	}
	firstUser := -1
	var content []any
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role == "user" {
			blocks, ok := message["content"].([]any)
			if ok {
				firstUser, content = i, blocks
			}
			break
		}
	}
	if firstUser < 0 || len(content) <= len(queried) {
		return false
	}
	want := make([]any, len(queried))
	for i, block := range queried {
		want[i] = plainBlock(block)
	}
	starts := make([]int, 0, 1)
	for k := 0; k+len(queried) <= len(content); k++ {
		matches := true
		for j := range queried {
			if !reflect.DeepEqual(plainBlock(content[k+j]), want[j]) {
				matches = false
				break
			}
		}
		if matches {
			starts = append(starts, k)
		}
	}
	if len(starts) != 1 || starts[0] == 0 {
		return false
	}
	k := starts[0]
	for _, raw := range content[:k] {
		block, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		if kind, _ := block["type"].(string); kind != "text" {
			return false
		}
	}
	reordered := make([]any, 0, len(content))
	reordered = append(reordered, content[k:k+len(queried)]...)
	reordered = append(reordered, content[:k]...)
	reordered = append(reordered, content[k+len(queried):]...)
	messages[firstUser].(map[string]any)["content"] = reordered
	return true
}

// pinMessageBreakpoint keeps the single message-level cache_control on content
// the next request replays unchanged.
//
// What does recur is known without reading native's text: everything through the
// last assistant message, plus the leading blocks of the newest turn that equal
// the frame the client queried. The first block native added or changed ends
// that span. When that block is a tool_result (parallel calls, native's reminder
// on the last result), a breakpoint on the unchanged results before it measured
// no cache hit even though they replay identically, so the span ends at the
// preceding assistant message — which is exactly why restoreQueriedTurn runs
// first: after restoration the tool_result does match and the breakpoint can
// land on it.
//
// Marks after the span are folded into one on its last block; a native mark
// inside the span recurs and stays. The breakpoint count never grows (Anthropic
// allows four across tools, system and messages), tools and system marks are
// never touched, and the moved marker keeps native's own TTL. Any payload that
// does not parse forwards as is.
func pinMessageBreakpoint(payload []byte, queried []any) []byte {
	if len(queried) == 0 {
		return payload
	}
	body, err := decodeWireBody(payload)
	if err != nil {
		return payload
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return payload
	}
	reordered := frameFirst(messages, queried)

	blocks := make([]wireBlockRef, 0, len(messages))
	marked := make([]wireBlockRef, 0, 2)
	lastAssistant := -1
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role == "assistant" {
			lastAssistant = i
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for j, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ref := wireBlockRef{message: i, index: j, block: block}
			blocks = append(blocks, ref)
			if _, has := block["cache_control"]; has {
				marked = append(marked, ref)
			}
		}
	}
	// Native always marks something in `messages` (the trailing per-request context
	// message at minimum); with nothing marked there is no directive to move, and a
	// mark this file invented could push the request past Anthropic's four-breakpoint
	// limit, so the body forwards as is.
	if len(marked) == 0 {
		if reordered {
			return reencodeOrOriginal(body, payload)
		}
		return payload
	}

	stable := make([]wireBlockRef, 0, len(blocks))
	for _, ref := range blocks {
		if ref.message <= lastAssistant {
			stable = append(stable, ref)
		}
	}
	if newestIndex := lastAssistant + 1; newestIndex < len(messages) {
		if newest, ok := messages[newestIndex].(map[string]any); ok {
			if role, _ := newest["role"].(string); role == "user" {
				if content, ok := newest["content"].([]any); ok {
					prefix := make([]wireBlockRef, 0, len(queried))
					for j := 0; j < len(content) && j < len(queried); j++ {
						if !reflect.DeepEqual(plainBlock(content[j]), plainBlock(queried[j])) {
							break
						}
						block, ok := content[j].(map[string]any)
						if !ok {
							break
						}
						prefix = append(prefix, wireBlockRef{message: newestIndex, index: j, block: block})
					}
					rest := content[len(prefix):]
					restStartsWithToolResult := false
					if len(rest) > 0 {
						if first, ok := rest[0].(map[string]any); ok {
							kind, _ := first["type"].(string)
							restStartsWithToolResult = kind == "tool_result"
						}
					}
					if !restStartsWithToolResult {
						stable = append(stable, prefix...)
					}
				}
			}
		}
	}

	var target *wireBlockRef
	for i := len(stable) - 1; i >= 0; i-- {
		kind, _ := stable[i].block["type"].(string)
		if !uncacheable(kind) {
			target = &stable[i]
			break
		}
	}
	if target == nil {
		if reordered {
			return reencodeOrOriginal(body, payload)
		}
		return payload
	}

	after := make([]wireBlockRef, 0, len(marked))
	for _, ref := range marked {
		if target.before(ref) {
			after = append(after, ref)
		}
	}
	if len(after) > 0 {
		moved := after[0].block["cache_control"]
		for _, ref := range after {
			delete(ref.block, "cache_control")
		}
		if _, has := target.block["cache_control"]; !has {
			target.block["cache_control"] = moved
		}
	} else if !reordered {
		return payload
	}
	return reencodeOrOriginal(body, payload)
}

// reencodeOrOriginal serialises an edited body and falls back to the untouched
// payload when encoding fails.
func reencodeOrOriginal(body map[string]any, original []byte) []byte {
	encoded, err := encodeWireBody(body)
	if err != nil {
		return original
	}
	return encoded
}

// restoreQueriedTurn restores the tool-result frame to the representation the
// client sent, without dropping protected content.
//
// The CLI echoes the queried frame back with its own additions (a trailing
// reminder inside the last tool result, split or joined text runs). Those
// additions never recur, so the frame — and with it the whole cached prefix —
// stops matching on the next request. Only trailing bare text is removable;
// media, metadata and cache directives are never crossed for matching. Returns
// errQueriedTurnMismatch when the frame cannot be proven losslessly
// reconcilable.
func restoreQueriedTurn(payload []byte, queried []any) ([]byte, error) {
	if len(queried) == 0 {
		return payload, nil
	}
	strict := false
	for _, item := range queried {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := block["type"].(string); kind == "tool_result" {
			strict = true
			break
		}
	}
	// An ordinary user-only frame is outside restoration scope, byte for byte.
	if !strict {
		return payload, nil
	}

	body, err := decodeWireBody(payload)
	if err != nil {
		return payload, errQueriedTurnMismatch
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		return payload, errQueriedTurnMismatch
	}
	lastAssistant := -1
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role == "assistant" {
			lastAssistant = i
		}
	}
	if lastAssistant+1 >= len(messages) {
		return payload, errQueriedTurnMismatch
	}
	newest, ok := messages[lastAssistant+1].(map[string]any)
	if !ok {
		return payload, errQueriedTurnMismatch
	}
	if role, _ := newest["role"].(string); role != "user" {
		return payload, errQueriedTurnMismatch
	}
	native, ok := newest["content"].([]any)
	if !ok {
		return payload, errQueriedTurnMismatch
	}

	resultIDs := make([]string, 0, len(queried))
	for _, item := range queried {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := block["type"].(string); kind != "tool_result" {
			continue
		}
		id, ok := block["tool_use_id"].(string)
		if !ok || id == "" {
			return payload, errQueriedTurnMismatch
		}
		resultIDs = append(resultIDs, id)
	}
	seen := make(map[string]bool, len(resultIDs))
	for _, id := range resultIDs {
		if seen[id] {
			return payload, errQueriedTurnMismatch
		}
		seen[id] = true
	}
	nativeIDs := make([]string, 0, len(native))
	for _, item := range native {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := block["type"].(string); kind != "tool_result" {
			continue
		}
		id, _ := block["tool_use_id"].(string)
		nativeIDs = append(nativeIDs, id)
	}
	// Unique ordered result IDs anchor the complete frame: no substring search
	// can accidentally choose another span.
	if !reflect.DeepEqual(resultIDs, nativeIDs) {
		return payload, errQueriedTurnMismatch
	}

	restored, err := restoreSequence(native, queried)
	if err != nil {
		return payload, err
	}
	if reflect.DeepEqual(native, restored) {
		return payload, nil
	}
	newest["content"] = restored
	encoded, err := encodeWireBody(body)
	if err != nil {
		return payload, errQueriedTurnMismatch
	}
	return encoded, nil
}

// restoreSequence walks the frame the client sent against what the CLI emitted,
// restoring each block. Split/joined bare text runs are equivalent for matching,
// but never across media, metadata or cache directives.
func restoreSequence(sent, host []any) ([]any, error) {
	restored := make([]any, 0, len(host))
	i, j := 0, 0
	for i < len(host) {
		if j >= len(sent) {
			return nil, errQueriedTurnMismatch
		}
		hostText, hostBare := bareTextBlock(host[i])
		sentText, sentBare := bareTextBlock(sent[j])
		if hostBare && sentBare {
			hostEnd, sentEnd := i+1, j+1
			original, actual := hostText, sentText
			for hostEnd < len(host) {
				text, ok := bareTextBlock(host[hostEnd])
				if !ok {
					break
				}
				original += text
				hostEnd++
			}
			for sentEnd < len(sent) {
				text, ok := bareTextBlock(sent[sentEnd])
				if !ok {
					break
				}
				actual += text
				sentEnd++
			}
			// Extensions before a protected host block are insertions, not suffixes.
			if hostEnd == len(host) {
				if !strings.HasPrefix(actual, original) {
					return nil, errQueriedTurnMismatch
				}
			} else if actual != original {
				return nil, errQueriedTurnMismatch
			}
			restored = append(restored, deepCopyValue(host[i:hostEnd]).([]any)...)
			i, j = hostEnd, sentEnd
			continue
		}
		block, err := restoreBlock(sent[j], host[i])
		if err != nil {
			return nil, err
		}
		restored = append(restored, block)
		i++
		j++
	}
	for ; j < len(sent); j++ {
		if _, ok := bareTextBlock(sent[j]); !ok {
			return nil, errQueriedTurnMismatch
		}
	}
	return restored, nil
}

// restoreBlock restores one block. A tool_result keeps its identity fields
// (`tool_use_id`, `is_error`) and only its content is reconciled; any other
// block must match except for the cache directive.
func restoreBlock(sent, host any) (any, error) {
	sentBlock, ok := sent.(map[string]any)
	if !ok {
		return nil, errQueriedTurnMismatch
	}
	hostBlock, ok := host.(map[string]any)
	if !ok {
		return nil, errQueriedTurnMismatch
	}
	restored := deepCopyValue(hostBlock).(map[string]any)

	if kind, _ := hostBlock["type"].(string); kind == "tool_result" {
		sentError, sentOK := protocolBool(sentBlock, "is_error")
		hostError, hostOK := protocolBool(hostBlock, "is_error")
		// Missing means false; a numeric 0 is not a protocol boolean.
		if !sentOK || !hostOK || sentError != hostError {
			return nil, errQueriedTurnMismatch
		}
		if !reflect.DeepEqual(
			withoutKeys(sentBlock, "content", "is_error", "cache_control"),
			withoutKeys(hostBlock, "content", "is_error", "cache_control"),
		) {
			return nil, errQueriedTurnMismatch
		}
		_, sentHasContent := sentBlock["content"]
		_, hostHasContent := hostBlock["content"]
		if sentHasContent != hostHasContent {
			return nil, errQueriedTurnMismatch
		}
		if hostHasContent {
			content, err := restoreContent(sentBlock["content"], hostBlock["content"])
			if err != nil {
				return nil, err
			}
			restored["content"] = content
		}
	} else if !reflect.DeepEqual(plainBlock(sentBlock), plainBlock(hostBlock)) {
		return nil, errQueriedTurnMismatch
	}

	if cached, has := sentBlock["cache_control"]; has {
		if hostCached, hostHas := hostBlock["cache_control"]; hostHas && !reflect.DeepEqual(cached, hostCached) {
			return nil, errQueriedTurnMismatch
		}
		restored["cache_control"] = cached
	}
	return restored, nil
}

// restoreContent reconciles a tool_result body. Scalar/list equivalence is
// allowed only for text carrying no metadata and no markers.
func restoreContent(sent, host any) (any, error) {
	_, hostIsText := host.(string)
	_, sentIsText := sent.(string)
	if hostIsText || sentIsText {
		left, leftOK := textOf(sent)
		right, rightOK := textOf(host)
		if !leftOK || !rightOK || !strings.HasPrefix(left, right) {
			return nil, errQueriedTurnMismatch
		}
		return deepCopyValue(host), nil
	}
	sentList, sentListOK := sent.([]any)
	hostList, hostListOK := host.([]any)
	if sentListOK && hostListOK {
		return restoreSequence(sentList, hostList)
	}
	if !reflect.DeepEqual(sent, host) {
		return nil, errQueriedTurnMismatch
	}
	return deepCopyValue(host), nil
}

// protocolBool reads a protocol boolean: absent means false, and any non-boolean
// value (notably a numeric 0) is a mismatch rather than a falsy.
func protocolBool(block map[string]any, key string) (value bool, ok bool) {
	raw, has := block[key]
	if !has {
		return false, true
	}
	typed, isBool := raw.(bool)
	return typed, isBool
}

// queriedFrame is the frame the client asked the model to answer: the last user
// frame this transport replayed. The cache-wire helpers compare it against what
// the CLI actually sent, so it is normalised through the same decoder as a
// request body — numbers become json.Number on both sides and equality holds.
func queriedFrame(frames []nativeFrame) []any {
	if len(frames) == 0 {
		return nil
	}
	last := frames[len(frames)-1]
	if last.Type != "user" {
		return nil
	}
	content, has := last.Message["content"]
	if !has {
		return nil
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var blocks []any
	if err := decoder.Decode(&blocks); err != nil {
		return nil
	}
	return blocks
}
