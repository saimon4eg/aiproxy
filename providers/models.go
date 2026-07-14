package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/whtsky/copilot2api/internal/upstream"
)

// ModelsCache caches the aggregated model list from all providers.
type ModelsCache struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex // serializes Refresh without blocking readers (avoids re-entrant c.mu deadlock)
	data      []byte
	ttl       time.Time
	config    *Config
}

// NewModelsCache creates a new ModelsCache.
func NewModelsCache(cfg *Config) *ModelsCache {
	return &ModelsCache{
		config: cfg,
	}
}

// ServeHTTP handles GET /v1/models.
func (c *ModelsCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	expired := time.Now().After(c.ttl)
	c.mu.RUnlock()

	if expired {
		// Serialize refreshes with a dedicated mutex. Refresh locks c.mu
		// internally for the final write, so holding c.mu here would deadlock
		// (RWMutex is not re-entrant). refreshMu also prevents a thundering
		// herd, and the slow upstream fetch never blocks readers on c.mu.
		c.refreshMu.Lock()
		c.mu.RLock()
		stillExpired := time.Now().After(c.ttl)
		c.mu.RUnlock()
		if stillExpired {
			if err := c.Refresh(r.Context()); err != nil {
				slog.Error("models cache refresh failed", "error", err)
				// Keep old cache, don't update ttl.
			}
		}
		c.refreshMu.Unlock()
	}

	c.mu.RLock()
	data := c.data
	c.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// Refresh rebuilds the aggregated model list from all providers.
func (c *ModelsCache) Refresh(ctx context.Context) error {
	var allModels []ModelInfo

	for _, p := range c.config.Providers {
		if p.Type == "copilot" {
			continue // Copilot models are fetched separately via FetchCopilotModels.
		}
		models, err := FetchModels(ctx, p)
		if err != nil {
			slog.Warn("failed to fetch models", "provider", p.ProviderID, "error", err)
			continue
		}
		for _, m := range models {
			// Add prefix to model ID.
			m.ID = p.ModelPrefix + "-" + m.ID
			// Object is always "model"; vendor defaults to provider_id (override if set).
			m.Object = "model"
			if m.Vendor == "" {
				m.Vendor = p.ProviderID
			}
			// Auto-populate supported_endpoints based on flags.
			populateEndpoints(&m, p)
			allModels = append(allModels, m)
		}
	}

	// Fetch Copilot models and add "copilot-" prefix.
	copilotModels, err := FetchCopilotModels(ctx, c.config)
	if err != nil {
		slog.Warn("failed to fetch copilot models", "error", err)
	} else {
		for i := range copilotModels {
			copilotModels[i].ID = "copilot-" + copilotModels[i].ID
		}
		// Normalise: /responses → /v1/responses, drop ws: endpoints.
		// Then augment: if /messages native → add /responses (via linguafranca),
		// and vice versa.   — applies everywhere.
		for i := range copilotModels {
			eps := copilotModels[i].SupportedEndpoints
			normalized := make([]string, 0, len(eps))
			for _, ep := range eps {
				if strings.HasPrefix(ep, "ws:") {
					continue // not supported
				}
				if ep == "/responses" {
					normalized = append(normalized, "/v1/responses")
				} else {
					normalized = append(normalized, ep)
				}
			}
			copilotModels[i].SupportedEndpoints = normalized

			hasMessages := supportsEndpointNorm(normalized, "/v1/messages")
			hasResponses := supportsEndpointNorm(normalized, "/v1/responses")
			if hasMessages && !hasResponses {
				copilotModels[i].SupportedEndpoints = append(normalized, "/v1/responses")
			}
			if hasResponses && !hasMessages {
				copilotModels[i].SupportedEndpoints = append(normalized, "/v1/messages")
			}
		}
		allModels = append(allModels, copilotModels...)
	}

	resp := ModelsListResponse{Object: "list", Data: allModels}
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal models: %w", err)
	}

	c.mu.Lock()
	c.data = data
	c.ttl = time.Now().Add(60 * time.Second)
	c.mu.Unlock()

	slog.Debug("models cache refreshed", "count", len(allModels))
	return nil
}

// FetchModels retrieves models for a single provider.
func FetchModels(ctx context.Context, p ProviderConfig) ([]ModelInfo, error) {
	// Static model list takes priority.
	if len(p.Models) > 0 {
		var models []ModelInfo
		if err := json.Unmarshal(mergeRawMessages(p.Models), &models); err != nil {
			return nil, fmt.Errorf("failed to parse static models for %s: %w", p.ProviderID, err)
		}
		return models, nil
	}

	// Dynamic fetch from provider API.
	if p.BaseURL == "" {
		return nil, fmt.Errorf("provider %s: no base_url or static models configured", p.ProviderID)
	}

	modelsURL := p.BaseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, "GET", modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %w", p.ProviderID, err)
	}

	// Set auth header.
	if tp := p.TokenProvider(); tp != nil {
		tok, err := tp.GetAccessToken()
		if err != nil {
			return nil, fmt.Errorf("provider %s: failed to get access token for models fetch: %w", p.ProviderID, err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	} else {
		switch p.Type {
		case "anthropic":
			req.Header.Set("x-api-key", p.APIKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		case "openai":
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
	}

	client := &http.Client{Timeout: 30 * time.Second}
	if p.ProxyHost != "" {
		// proxy_host is validated in Validate(); parse cannot fail here.
		u, _ := upstream.ParseProxyURL(p.ProxyHost)
		client.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch models from %s: %w", p.ProviderID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider %s returned %d", p.ProviderID, resp.StatusCode)
	}

	// Try to parse as ModelsListResponse first, then as OpenAI format.
	var list ModelsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		// Try OpenAI format: {"object":"list","data":[{"id":"...","object":"model",...}]}
		return nil, fmt.Errorf("failed to parse models from %s: %w", p.ProviderID, err)
	}

	return list.Data, nil
}

// FetchCopilotModels retrieves models from Copilot API via the copilot handler.
func FetchCopilotModels(ctx context.Context, cfg *Config) ([]ModelInfo, error) {
	// Use the copilot handler's /v1/models endpoint internally.
	req, err := http.NewRequestWithContext(ctx, "GET", "/v1/models", nil)
	if err != nil {
		return nil, err
	}

	rec := &responseRecorder{header: make(http.Header)}
	cfg.copilotHandler.ServeHTTP(rec, req)

	if rec.statusCode != http.StatusOK && rec.statusCode != 0 {
		return nil, fmt.Errorf("copilot models returned %d", rec.statusCode)
	}

	var raw struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse copilot models: %w", err)
	}

	// Parse each model fully to preserve all fields.
	var models []ModelInfo
	for _, rm := range raw.Data {
		var m ModelInfo
		if err := json.Unmarshal(rm, &m); err != nil {
			slog.Warn("failed to parse copilot model", "error", err)
			continue
		}
		models = append(models, m)
	}

	// Cache per-model endpoint support for capability-driven routing.
	cfg.copilotModels = make(map[string]copilotEndpoints, len(models))
	for _, m := range models {
		cfg.copilotModels[m.ID] = copilotEndpoints{
			supportsMessages:  supportsEndpointNorm(m.SupportedEndpoints, "/v1/messages"),
			supportsResponses: supportsEndpointNorm(m.SupportedEndpoints, "/v1/responses"),
		}
	}

	return models, nil
}

// populateEndpoints adds supported endpoints based on provider flags.
func populateEndpoints(m *ModelInfo, p ProviderConfig) {
	switch p.Type {
	case "anthropic":
		// Always: /v1/messages (native).
		// convert_to_openai=true: also /v1/responses (via linguafranca).
		// Chat Completions is NOT added — router always returns 400.
		if p.ConvertToOpenAI {
			m.SupportedEndpoints = appendIfMissing(m.SupportedEndpoints, "/v1/responses")
		}
		if !containsEndpoint(m.SupportedEndpoints, "/v1/messages") {
			m.SupportedEndpoints = append(m.SupportedEndpoints, "/v1/messages")
		}
	case "openai":
		// Native: /v1/chat/completions and /v1/responses (router passthrough).
		// convert_to_anthropic=true: also /v1/messages (messages→responses conversion).
		m.SupportedEndpoints = appendIfMissing(m.SupportedEndpoints, "/v1/responses")
		if !containsEndpoint(m.SupportedEndpoints, "/v1/chat/completions") {
			m.SupportedEndpoints = append(m.SupportedEndpoints, "/v1/chat/completions")
		}
		if p.ConvertToAnthropic {
			m.SupportedEndpoints = appendIfMissing(m.SupportedEndpoints, "/v1/messages")
		}
	case "chat":
		// Native: /v1/chat/completions (router passthrough).
		// convert_to_anthropic=true: also /v1/messages (messages→chat conversion).
		m.SupportedEndpoints = appendIfMissing(m.SupportedEndpoints, "/v1/chat/completions")
		if p.ConvertToAnthropic {
			m.SupportedEndpoints = appendIfMissing(m.SupportedEndpoints, "/v1/messages")
		}
	}
}

func appendIfMissing(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}

func containsEndpoint(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// supportsEndpointNorm checks whether an endpoint list contains target,
// normalising the "/v1" prefix on both sides (tolerant to /v1/messages ↔ /messages).
func supportsEndpointNorm(endpoints []string, target string) bool {
	norm := strings.TrimPrefix(target, "/v1")
	for _, ep := range endpoints {
		if strings.TrimPrefix(ep, "/v1") == norm {
			return true
		}
	}
	return false
}

// mergeRawMessages merges multiple json.RawMessage into one JSON array.
func mergeRawMessages(msgs []json.RawMessage) json.RawMessage {
	if len(msgs) == 0 {
		return json.RawMessage("[]")
	}
	if len(msgs) == 1 {
		return msgs[0]
	}
	// Build a JSON array from individual messages.
	var buf []byte
	buf = append(buf, '[')
	for i, m := range msgs {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, m...)
	}
	buf = append(buf, ']')
	return json.RawMessage(buf)
}

// responseRecorder captures an HTTP response.
type responseRecorder struct {
	header     http.Header
	body       []byte
	statusCode int
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	return len(b), nil
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
}
