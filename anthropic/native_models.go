package anthropic

// NativeModel represents a native Anthropic API model.
type NativeModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// NativeModels is the list of native Anthropic models served when ANTHROPIC_ON=true.
// These are accessible only via Claude Code CLI (/v1/messages).
var NativeModels = []NativeModel{
	{ID: "claude-opus-4-7", Object: "model", OwnedBy: "anthropic"},
	{ID: "claude-sonnet-4-6", Object: "model", OwnedBy: "anthropic"},
	{ID: "claude-opus-4-6", Object: "model", OwnedBy: "anthropic"},
	{ID: "claude-haiku-4-5", Object: "model", OwnedBy: "anthropic"},
	{ID: "claude-sonnet-4-5", Object: "model", OwnedBy: "anthropic"},
}

// IsNativeAnthropic reports whether the model ID should be routed to the
// native Anthropic API. It matches bare "claude-" IDs — not "copilot-claude-"
// prefixed ones, which go to Copilot after prefix stripping.
func IsNativeAnthropic(modelID string) bool {
	return len(modelID) >= 7 && modelID[:7] == "claude-"
}
