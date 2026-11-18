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
	"github.com/whtsky/copilot2api/requestlog"
)

// TokenProvider allows a provider to supply OAuth bearer tokens at request time.
type TokenProvider interface {
	GetAccessToken() (string, error)
}

// ProviderConfig describes a single provider from providers.json.
type ProviderConfig struct {
	ProviderID    string `json:"provider_id"`
	Name          string `json:"name"`
	Type          string `json:"type"`               // "copilot", "messages", "responses", "chat"
	SubType       string `json:"sub_type,omitempty"` // e.g. "deepseek" within "messages"
	Auth          string `json:"auth"`               // "oauth", "api_key"
	Enabled       bool   `json:"enabled"`
	BaseURL       string `json:"base_url,omitempty"`
	APIKey        string `json:"api_key,omitempty"`
	ProxyHost     string `json:"proxy_host,omitempty"`
	ModelPrefix   string `json:"model_prefix"`
	IntegrationID string `json:"integration_id,omitempty"`
	APIVersion    string `json:"api_version,omitempty"`
	// ContextTier (copilot only): "" = auto (inject contextTier "long_context"
	// for models whose catalog window is >= 500k), "off" = never, any other
	// value = force it for every model on /responses and /chat/completions.
	ContextTier string `json:"context_tier,omitempty"`
	// ModelAliases (copilot only): client-visible model id -> Copilot model id
	// (without the "copilot-" prefix). nil = built-in default
	// (codex-auto-review -> gpt-5.6-luna); {} = no aliases.
	ModelAliases       map[string]string `json:"model_aliases,omitempty"`
	ConvertToResponses bool              `json:"convert_to_responses"`
	ConvertToMessages  bool              `json:"convert_to_messages"`
	AuthHeader         string            `json:"auth_header,omitempty"`       // "x-api-key" (default) or "bearer"
	APIPrefix          string            `json:"api_prefix,omitempty"`        // API version prefix, default "/v1"
	DisableKeepAlive   bool              `json:"disable_keepalive,omitempty"` // disable HTTP keep-alive (workaround for upstream HTTP/2 stream-after-nonstream bug)
	// claude_cli only: absolute path to the official Claude Code binary.
	// Default: CLAUDE_CLI_PATH, then "claude" on PATH, then the usual install dirs.
	Command string `json:"command,omitempty"`
	// claude_cli only: seconds of silence allowed before the turn is abandoned.
	ReadIdleSeconds int `json:"read_idle_seconds,omitempty"`
	// claude_cli only: directory holding the CLI's own login (CLAUDE_CONFIG_DIR).
	// Default: <token dir>/claude.
	ClaudeConfigDir string            `json:"claude_config_dir,omitempty"`
	Models          []json.RawMessage `json:"models,omitempty"`

	tokenProvider TokenProvider
	transport     *http.Transport
	client        *http.Client
}

// upstreamPath replaces the standard /v1 prefix with the provider's API prefix.
// Default is "/v1" — only override for non-standard APIs (e.g. Z.ai uses "/v4").
func (p *ProviderConfig) upstreamPath(standardPath string) string {
	if p.APIPrefix == "" || p.APIPrefix == "/v1" {
		return standardPath
	}
	prefix := p.APIPrefix
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return prefix + strings.TrimPrefix(standardPath, "/v1")
}

// SetTokenProvider attaches an OAuth token provider.
func (p *ProviderConfig) SetTokenProvider(tp TokenProvider) {
	p.tokenProvider = tp
}

// AdapterKey returns the lookup key for this provider's adapter:
// "type[subtype]" when SubType is set, otherwise just "type".
func (p *ProviderConfig) AdapterKey() string {
	if p.SubType != "" {
		return p.Type + "[" + p.SubType + "]"
	}
	return p.Type
}

// TokenProvider returns the OAuth token provider, if any.
func (p *ProviderConfig) TokenProvider() TokenProvider {
	return p.tokenProvider
}

// initHTTPClient creates the shared HTTP client for this provider.
// MUST be called once at startup (see LoadConfig), before any handler goroutine.
func (p *ProviderConfig) initHTTPClient() error {
	p.transport = &http.Transport{
		ResponseHeaderTimeout: 5 * time.Minute,
		DisableKeepAlives:     p.DisableKeepAlive,
	}
	if p.ProxyHost != "" {
		// A configured proxy is always applied — never a silent direct fallback.
		// Validate() already parsed this at startup; a failure here is a bug, not a
		// reason to quietly connect around the proxy.
		u, err := upstream.ParseProxyURL(p.ProxyHost)
		if err != nil {
			return fmt.Errorf("provider %s: invalid proxy_host %q: %w", p.ProviderID, p.ProxyHost, err)
		}
		p.transport.Proxy = http.ProxyURL(u)
	}
	p.client = &http.Client{
		Transport: requestlog.WrapTransport(p.transport),
		// No overall timeout: this is a proxy, and a long generation legitimately
		// outlives any fixed budget (a full 1M-context turn can stream for tens of
		// minutes). ResponseHeaderTimeout above still catches an upstream that
		// never answers, and every handler carries the client's own context, so a
		// disconnected client still cancels its upstream request.
		Timeout: 0,
	}
	return nil
}

// Config is the root structure from providers.json.
type Config struct {
	Providers               []ProviderConfig
	copilotHandler          http.Handler
	copilotAnthropicHandler http.Handler
	copilotModels           map[string]copilotEndpoints // key = model ID without "copilot-" prefix
	// disabledPrefixes remembers model prefixes of providers turned off in
	// providers.json, so a rejected model can be reported as "that provider is
	// disabled" instead of the misleading "unknown model".
	disabledPrefixes []string
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
			if p.ModelPrefix != "" {
				cfg.disabledPrefixes = append(cfg.disabledPrefixes, p.ModelPrefix)
			}
			continue
		}
		if p.ProviderID == "" {
			continue
		}
		// Default API prefix to /v1 if not set.
		if p.APIPrefix == "" {
			p.APIPrefix = "/v1"
		}
		cfg.Providers = append(cfg.Providers, *p)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// One shared HTTP client per provider, created at startup (single-threaded here)
	// to avoid a data race on lazy init from concurrent handler goroutines.
	for i := range cfg.Providers {
		if err := cfg.Providers[i].initHTTPClient(); err != nil {
			return nil, err
		}
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
		// claude-cli owns its credential itself: the CLI logs in natively into
		// its own config directory and refreshes there. aiproxy never holds,
		// refreshes or forwards a subscription token, so there is nothing to
		// initialise here.
		if p.Type == "claude-cli" {
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

		if p.Type != "copilot" && (p.ContextTier != "" || p.ModelAliases != nil) {
			return fmt.Errorf("provider %s: context_tier and model_aliases are only supported for type=copilot", p.ProviderID)
		}
		if err := validateModelAliases(p.ProviderID, p.ModelAliases); err != nil {
			return err
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
			if p.ConvertToResponses {
				return fmt.Errorf("provider %s: convert_to_responses not supported for type=copilot", p.ProviderID)
			}
			if p.ConvertToMessages {
				return fmt.Errorf("provider %s: convert_to_messages not supported for type=copilot", p.ProviderID)
			}
		case "messages":
			if p.ConvertToMessages {
				return fmt.Errorf("provider %s: convert_to_messages requires type=responses, got type=messages", p.ProviderID)
			}
		case "responses":
			if p.ConvertToResponses {
				return fmt.Errorf("provider %s: convert_to_responses requires type=messages, got type=responses", p.ProviderID)
			}
		case "chat":
			if p.ConvertToResponses {
				return fmt.Errorf("provider %s: convert_to_responses not supported for type=chat", p.ProviderID)
			}
		case "claude-cli":
			// The transport speaks Anthropic natively, so a Responses client is
			// served by the existing reverse converter, exactly as for a
			// type=messages provider. The other direction has no meaning here.
			if p.ConvertToMessages {
				return fmt.Errorf("provider %s: convert_to_messages not supported for type=claude-cli", p.ProviderID)
			}
			if p.BaseURL == "" {
				p.BaseURL = "https://api.anthropic.com"
			}
			if p.Auth == "oauth" && p.APIKey != "" {
				return fmt.Errorf("provider %s: api_key and auth=oauth are mutually exclusive", p.ProviderID)
			}
			// The relay appends the CLI's own /v1/messages to base_url, so a
			// non-standard api_prefix would silently produce a wrong upstream path.
			if p.APIPrefix != "" && p.APIPrefix != "/v1" {
				return fmt.Errorf("provider %s: api_prefix is not supported for type=claude-cli (the CLI always calls /v1/messages)", p.ProviderID)
			}
			if p.ModelPrefix == "" {
				return fmt.Errorf("provider %s: model_prefix is required for type=claude-cli", p.ProviderID)
			}
		default:
			return fmt.Errorf("provider %s: unknown type %q (must be copilot, messages, responses, chat, or claude-cli)", p.ProviderID, p.Type)
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

// IsCopilotEnabled returns true if any enabled copilot provider exists.
func (c *Config) IsCopilotEnabled() bool {
	for _, p := range c.Providers {
		if p.Type == "copilot" {
			return true
		}
	}
	return false
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
