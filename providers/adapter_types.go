package providers

import (
	"encoding/json"
	"sync"
)

// Adapter provides provider-specific request/response patches.
// Implementations live in providers/adapters/ to avoid circular imports.
type Adapter interface {
	// NormalizeTools adjusts tool definitions before sending upstream.
	NormalizeTools(tools []json.RawMessage) []json.RawMessage

	// PatchRequest mutates the request body before sending upstream.
	PatchRequest(body map[string]interface{}) map[string]interface{}

	// PatchResponse mutates the response body before returning to the client.
	PatchResponse(body map[string]interface{}) map[string]interface{}
}

// adapters holds registered provider adapters, keyed by provider_id.
// Managed from main.go via RegisterAdapter.
var (
	adapters   = map[string]Adapter{}
	adaptersMu sync.RWMutex
)

// RegisterAdapter registers an adapter for a provider.
func RegisterAdapter(providerID string, a Adapter) {
	adaptersMu.Lock()
	defer adaptersMu.Unlock()
	adapters[providerID] = a
}

// GetAdapter returns the adapter matching type[subtype], falling back to type.
// A provider with sub_type="deepseek" looks up "messages[deepseek]" first,
// then "messages". Returns a no-op adapter when nothing matches.
func GetAdapter(typ, subType string) Adapter {
	adaptersMu.RLock()
	defer adaptersMu.RUnlock()
	if subType != "" {
		if a, ok := adapters[typ+"["+subType+"]"]; ok {
			return a
		}
	}
	if a, ok := adapters[typ]; ok {
		return a
	}
	return &noopAdapter{}
}

// noopAdapter is the default adapter that does nothing.
type noopAdapter struct{}

func (a *noopAdapter) NormalizeTools(tools []json.RawMessage) []json.RawMessage   { return tools }
func (a *noopAdapter) PatchRequest(body map[string]interface{}) map[string]interface{}  { return body }
func (a *noopAdapter) PatchResponse(body map[string]interface{}) map[string]interface{} { return body }
