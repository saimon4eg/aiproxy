package adapters

import (
	"encoding/json"
	"strings"

	"github.com/whtsky/copilot2api/internal/modelid"
	"github.com/whtsky/copilot2api/internal/types"
	"github.com/whtsky/copilot2api/providers"
)

// DeepSeekAdapter implements providers.Adapter for DeepSeek.
type DeepSeekAdapter struct{}

// NewDeepSeekAdapter creates a new DeepSeekAdapter.
func NewDeepSeekAdapter() *DeepSeekAdapter {
	return &DeepSeekAdapter{}
}

// NormalizeTools adjusts DeepSeek tool definitions.
func (a *DeepSeekAdapter) NormalizeTools(tools []json.RawMessage) []json.RawMessage {
	return tools
}

// PatchRequest applies DeepSeek-specific request patches:
//   - Model name normalization: strips UI-only context suffixes (e.g. "[1m]"),
//     which DeepSeek does not understand, then collapses to the canonical id.
//   - MCP namespace expansion: converts "namespace"-typed tools to flat "function"
//     tools and registers short→full name mappings for stream post-processing.
func (a *DeepSeekAdapter) PatchRequest(body map[string]interface{}) map[string]interface{} {
	// Model name normalization.
	if model, ok := body["model"].(string); ok {
		body["model"] = normalizeDeepSeekModel(model)
	}

	// MCP namespace expansion.
	tools, _ := body["tools"].([]interface{})
	if tools == nil {
		return body
	}
	var expanded []interface{}
	for _, t := range tools {
		tm, ok := t.(map[string]interface{})
		if !ok {
			expanded = append(expanded, t)
			continue
		}
		if tm["type"] == "namespace" {
			ns, _ := tm["namespace"].(string)
			if ns == "" {
				ns, _ = tm["name"].(string)
			}
			if !strings.HasPrefix(ns, "mcp__") {
				expanded = append(expanded, t)
				continue
			}
			nested, _ := tm["tools"].([]interface{})
			for _, nt := range nested {
				ntm, ok := nt.(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := ntm["name"].(string)
				if name == "" {
					continue
				}
				// Register short→full mapping for stream post-processing.
				types.RegisterMcpToolName(name, ns+name)
				expanded = append(expanded, map[string]interface{}{
					"type":        "function",
					"name":        name,
					"description": ntm["description"],
					"parameters":  ntm["parameters"],
				})
			}
		} else {
			expanded = append(expanded, t)
		}
	}
	body["tools"] = expanded
	return body
}

// PatchResponse stores response ID for potential reconnection.
func (a *DeepSeekAdapter) PatchResponse(body map[string]interface{}) map[string]interface{} {
	return body
}

// normalizeDeepSeekModel canonicalizes a DeepSeek model id:
//   - strips UI-only display context suffixes via the shared modelid helper
//     (reusable across providers) — DeepSeek does not support 1M-style suffixes;
//   - collapses any remaining variant to the canonical base id (DeepSeek-specific).
func normalizeDeepSeekModel(model string) string {
	normalized := modelid.StripDisplayContextSuffixes(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(normalized, "deepseek-v4-pro"):
		return "deepseek-v4-pro"
	case strings.HasPrefix(normalized, "deepseek-v4-flash"):
		return "deepseek-v4-flash"
	default:
		return normalized
	}
}

var _ providers.Adapter = (*DeepSeekAdapter)(nil)
