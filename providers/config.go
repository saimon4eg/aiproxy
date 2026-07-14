package providers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/whtsky/copilot2api/internal/upstream"
)

// TokenProvider allows a provider to supply OAuth bearer tokens at request time.
type TokenProvider interface {
	GetAccessToken() (string, error)
}

// ProviderConfig describes a single provider from providers.json.
type ProviderConfig struct {
	ProviderID                 string            `json:"provider_id"`
	Name                       string            `json:"name"`
	Type                       string            `json:"type"` // "copilot", "anthropic", "openai"
	Auth                       string            `json:"auth"` // "oauth", "api_key"
	Enabled                    bool              `json:"enabled"`
	BaseURL                    string            `json:"base_url,omitempty"`
	APIKey                     string            `json:"api_key,omitempty"`
	ProxyHost                  string            `json:"proxy_host,omitempty"`
	ModelPrefix                string            `json:"model_prefix"`
	IntegrationID              string            `json:"integration_id,omitempty"`
	APIVersion                 string            `json:"api_version,omitempty"`
	ConvertToOpenAI    bool              `json:"convert_to_openai"`
	ConvertToAnthropic bool              `json:"convert_to_anthropic"`
	AuthHeader         string            `json:"auth_header,omitempty"` // "x-api-key" (default) or "bearer"
	Models                     []json.RawMessage `json:"models,omitempty"`

	tokenProvider TokenProvider
	transport     *http.Transport
	client        *http.Client
}

// SetTokenProvider attaches an OAuth token provider.
func (p *ProviderConfig) SetTokenProvider(tp TokenProvider) {
	p.tokenProvider = tp
}

// TokenProvider returns the OAuth token provider, if any.
func (p *ProviderConfig) TokenProvider() TokenProvider {
	return p.tokenProvider
}

// initHTTPClient creates the shared HTTP client for this provider.
// MUST be called once at startup (see LoadConfig), before any handler goroutine.
func (p *ProviderConfig) initHTTPClient() {
	p.transport = &http.Transport{
		ResponseHeaderTimeout: 5 * time.Minute,
	}
	if p.ProxyHost != "" {
		// proxy_host is validated in Validate(); parse cannot fail here.
		// A configured proxy is always applied — no silent direct fallback.
		u, _ := upstream.ParseProxyURL(p.ProxyHost)
		p.transport.Proxy = http.ProxyURL(u)
	}
	p.client = &http.Client{Transport: p.transport, Timeout: 5 * time.Minute}
}

// Config is the root structure from providers.json.
type Config struct {
	Providers               []ProviderConfig
	copilotHandler          http.Handler
	copilotAnthropicHandler http.Handler
	copilotModels           map[string]copilotEndpoints // key = model ID without "copilot-" prefix
}

// copilotEndpoints caches which endpoints a Copilot model supports natively.
type copilotEndpoints struct {
	supportsMessages  bool
	supportsResponses bool
}

// SetCopilotAnthropicHandler wires the Anthropic Messages handler for copilot-*
// models on /v1/messages (Anthropic→Responses conversion via Copilot), matching
// upstream copilot2api routing. Without it, /v1/messages for copilot models 404s.
func (c *Config) SetCopilotAnthropicHandler(h http.Handler) {
	c.copilotAnthropicHandler = h
}

// OAuthInitFunc initializes OAuth for a given provider.
// providerID and proxyHost come from providers.json.
type OAuthInitFunc func(providerID string, proxyHost string) (TokenProvider, error)

// LoadConfig loads provider configuration from a JSON file.
// Returns error if the file doesn't exist — providers.json is mandatory.
func LoadConfig(path string, copilotHandler http.Handler) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("providers.json is required but could not be read: %w", err)
	}

	var raw struct {
		Providers []ProviderConfig `json:"providers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}

	cfg := &Config{copilotHandler: copilotHandler}
	for i := range raw.Providers {
		p := &raw.Providers[i]
		if !p.Enabled {
			continue
		}
		if p.ProviderID == "" {
			continue
		}
		cfg.Providers = append(cfg.Providers, *p)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// One shared HTTP client per provider, created at startup (single-threaded here)
	// to avoid a data race on lazy init from concurrent handler goroutines.
	for i := range cfg.Providers {
		cfg.Providers[i].initHTTPClient()
	}

	slog.Info("providers loaded", "count", len(cfg.Providers))
	return cfg, nil
}

// InitOAuth initializes OAuth for all enabled providers with auth="oauth".
func (c *Config) InitOAuth(init OAuthInitFunc) {
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Auth != "oauth" || !p.Enabled {
			continue
		}
		// copilot uses its own auth stack (GitHub device flow via auth.Client);
		// skip OAuth init to avoid false WARN.
		if p.Type == "copilot" {
			continue
		}
		tp, err := init(p.ProviderID, p.ProxyHost)
		if err != nil {
			slog.Warn("oauth init failed", "provider", p.ProviderID, "error", err)
			continue
		}
		p.SetTokenProvider(tp)
		slog.Info("oauth initialized", "provider", p.ProviderID)
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	seenPrefixes := make(map[string]string)

	for i := range c.Providers {
		p := &c.Providers[i]

		if p.ProviderID == "" {
			return fmt.Errorf("provider[%d]: provider_id is required", i)
		}
		if p.Type == "" {
			return fmt.Errorf("provider %s: type is required", p.ProviderID)
		}
		if p.ModelPrefix == "" {
			return fmt.Errorf("provider %s: model_prefix is required", p.ProviderID)
		}

		// api_key and auth=oauth are mutually exclusive.
		if p.APIKey != "" && p.Auth == "oauth" {
			return fmt.Errorf("provider %s: api_key and auth=oauth are mutually exclusive", p.ProviderID)
		}

		if p.Auth != "oauth" && p.APIKey == "" && p.Type != "copilot" {
			return fmt.Errorf("provider %s: api_key is required when auth is not oauth", p.ProviderID)
		}

		// A configured proxy_host must be usable — fail fast at startup rather
		// than silently falling back to a direct connection at request time.
		if p.ProxyHost != "" {
			if _, err := upstream.ParseProxyURL(p.ProxyHost); err != nil {
				return fmt.Errorf("provider %s: invalid proxy_host %q: %w", p.ProviderID, p.ProxyHost, err)
			}
		}

		if strings.HasPrefix(p.ModelPrefix, "copilot-") {
			return fmt.Errorf("provider %s: model_prefix %q starts with reserved prefix 'copilot-'", p.ProviderID, p.ModelPrefix)
		}

		if existing, ok := seenPrefixes[p.ModelPrefix]; ok {
			return fmt.Errorf("provider %s: duplicate model_prefix %q (already used by %s)", p.ProviderID, p.ModelPrefix, existing)
		}
		seenPrefixes[p.ModelPrefix] = p.ProviderID

		switch p.Type {
		case "copilot":
			if p.ConvertToOpenAI {
				return fmt.Errorf("provider %s: convert_to_openai not supported for type=copilot", p.ProviderID)
			}
			if p.ConvertToAnthropic {
				return fmt.Errorf("provider %s: convert_to_anthropic not supported for type=copilot", p.ProviderID)
			}
		case "anthropic":
			if p.ConvertToAnthropic {
				return fmt.Errorf("provider %s: convert_to_anthropic requires type=openai, got type=anthropic", p.ProviderID)
			}
		case "openai":
			if p.ConvertToOpenAI {
				return fmt.Errorf("provider %s: convert_to_openai requires type=anthropic, got type=openai", p.ProviderID)
			}
		case "chat":
			if p.ConvertToOpenAI {
				return fmt.Errorf("provider %s: convert_to_openai not supported for type=chat", p.ProviderID)
			}
		default:
			return fmt.Errorf("provider %s: unknown type %q (must be copilot, anthropic, openai, or chat)", p.ProviderID, p.Type)
		}
	}

	return nil
}

// FindProvider returns the provider matching the given model ID by prefix.
func (c *Config) FindProvider(model string) *ProviderConfig {
	for i := range c.Providers {
		p := &c.Providers[i]
		prefix := p.ModelPrefix + "-"
		if len(model) > len(prefix) && model[:len(prefix)] == prefix {
			return p
		}
	}
	return nil
}

// ByID returns the provider with the given provider_id, or nil.
func (c *Config) ByID(providerID string) *ProviderConfig {
	for i := range c.Providers {
		if c.Providers[i].ProviderID == providerID {
			return &c.Providers[i]
		}
	}
	return nil
}
