package providers

// Routing of Anthropic-shaped providers.
//
// There is no special "native Anthropic" path any more: an id reaches a provider
// only through its model_prefix, exactly like every other provider in this repo.
// Two shapes are covered here — a type=messages provider with an API key, whose
// prefix is stripped before the request goes upstream, and the claude-cli
// provider, which keeps the id native because the official CLI receives it.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// routeAndReadModel runs Route for the given model and returns the model that
// the outgoing request body carries (prefix transformations applied).
func routeAndReadModel(t *testing.T, cfg *Config, model string) (string, error) {
	t.Helper()

	body, err := json.Marshal(map[string]any{"model": model, "max_tokens": 16})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))

	h, err := cfg.Route(req)
	if err != nil {
		return "", err
	}
	if h == nil {
		t.Fatal("Route returned a nil handler without an error")
	}

	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read rewritten body: %v", err)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal rewritten body: %v", err)
	}
	return out.Model, nil
}

// TestRoute_BareClaudeIsNotCaptured: a bare claude-* id must not be picked up by
// a provider whose prefix is something else. This is the behaviour that replaced
// the removed by-id lookup for the old "anthropic" provider: with nothing
// matching, the client gets a plain "unknown model" instead of a silent route to
// whatever provider happened to be called "anthropic".
func TestRoute_BareClaudeIsNotCaptured(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{
				ProviderID:  "anthropic-api",
				Type:        "messages",
				ModelPrefix: "anthropic-api",
				Enabled:     true,
				BaseURL:     "https://api.anthropic.com",
				APIKey:      "sk-test",
			},
		},
	}

	if _, err := routeAndReadModel(t, cfg, "claude-opus-5-5"); err == nil {
		t.Fatal("bare claude-* must not route to a provider with a different prefix")
	}
}

// TestRoute_MessagesProviderStripsPrefix: an API-key Anthropic provider behaves
// like any other messages provider — prefix off, native id upstream.
func TestRoute_MessagesProviderStripsPrefix(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{
				ProviderID:  "anthropic-api",
				Type:        "messages",
				ModelPrefix: "anthropic-api",
				Enabled:     true,
				BaseURL:     "https://api.anthropic.com",
				APIKey:      "sk-test",
			},
		},
	}

	got, err := routeAndReadModel(t, cfg, "anthropic-api-claude-opus-5-5")
	if err != nil {
		t.Fatalf("prefixed id must route via FindProvider, got error: %v", err)
	}
	if got != "claude-opus-5-5" {
		t.Errorf("forwarded model = %q, want the %q prefix stripped", got, "anthropic-api-")
	}
}

// TestRoute_ClaudeCLIKeepsNativeID: the subscription provider advertises bare
// ids (the model list does not add a prefix for claude-cli) and the router hands
// the id to the official CLI untouched — stripping "claude-" would leave
// "sonnet-5-5", which the CLI does not know.
func TestRoute_ClaudeCLIKeepsNativeID(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{
				ProviderID:  "anthropic-claude",
				Type:        "claude-cli",
				Auth:        "oauth",
				ModelPrefix: "claude",
				Enabled:     true,
				BaseURL:     "https://api.anthropic.com",
			},
		},
	}

	got, err := routeAndReadModel(t, cfg, "claude-sonnet-5-5")
	if err != nil {
		t.Fatalf("bare claude-* must reach the claude-cli provider, got error: %v", err)
	}
	if got != "claude-sonnet-5-5" {
		t.Errorf("forwarded model = %q, want it unchanged", got)
	}
}

// TestRoute_DisabledClaudeCLIReportsReason keeps the refusal honest: a disabled
// provider is dropped by LoadConfig and only its prefix survives, so the router
// must say "you turned that provider off" rather than "unknown model".
func TestRoute_DisabledClaudeCLIReportsReason(t *testing.T) {
	// LoadConfig removes disabled providers from Providers and records the prefix.
	cfg := &Config{disabledPrefixes: []string{"claude"}}

	if _, err := routeAndReadModel(t, cfg, "claude-sonnet-5-5"); err == nil {
		t.Fatal("disabled provider must not route")
	}
	if got := cfg.disabledProviderFor("claude-sonnet-5-5"); got != "claude" {
		t.Errorf("disabledProviderFor = %q, want claude", got)
	}
}

// TestRoute_CopilotDisabledRefusesCleanly: copilot stays in the config as a
// disabled provider, and its models must answer "disabled" instead of reaching
// a handler with no credential and returning an opaque 500.
func TestRoute_CopilotDisabledRefusesCleanly(t *testing.T) {
	cfg := &Config{disabledPrefixes: []string{"copilot"}}

	body := bytes.NewReader([]byte(`{"model":"copilot-claude-sonnet-5-5","max_tokens":8}`))
	req := httptest.NewRequest("POST", "/v1/messages", body)

	if _, err := cfg.Route(req); err == nil {
		t.Fatal("disabled copilot must not route")
	} else if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("error = %q, want it to name the disabled provider", err)
	}
}
