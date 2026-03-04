package providers

// ModelInfo is a full model descriptor compatible with Copilot API /v1/models format.
// All fields are preserved during marshal/unmarshal — no data loss.
type ModelInfo struct {
	ID                 string             `json:"id"`
	Object             string             `json:"object"`
	Created            int64              `json:"created,omitempty"`
	OwnedBy            string             `json:"owned_by,omitempty"`
	Name               string             `json:"name,omitempty"`
	Vendor             string             `json:"vendor,omitempty"`
	SupportedEndpoints []string           `json:"supported_endpoints"`
	Capabilities       *ModelCapabilities `json:"capabilities,omitempty"`
}

// ModelCapabilities describes model capabilities.
type ModelCapabilities struct {
	Type     string        `json:"type"`
	Supports ModelSupports `json:"supports"`
	Limits   ModelLimits   `json:"limits"`
}

// ModelSupports lists supported feature flags.
type ModelSupports struct {
	Streaming         bool     `json:"streaming"`
	ToolCalls         bool     `json:"tool_calls"`
	ParallelToolCalls bool     `json:"parallel_tool_calls"`
	Vision            bool     `json:"vision"`
	StructuredOutputs bool     `json:"structured_outputs,omitempty"`
	ReasoningEffort   []string `json:"reasoning_effort,omitempty"`
	MaxThinkingBudget int      `json:"max_thinking_budget,omitempty"`
	MinThinkingBudget int      `json:"min_thinking_budget,omitempty"`
}

// ModelLimits describes model limits.
type ModelLimits struct {
	MaxContextWindowTokens int `json:"max_context_window_tokens,omitempty"`
	MaxOutputTokens        int `json:"max_output_tokens,omitempty"`
	MaxPromptTokens        int `json:"max_prompt_tokens,omitempty"`
}

// ModelsListResponse is the /v1/models response wrapper.
type ModelsListResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}
