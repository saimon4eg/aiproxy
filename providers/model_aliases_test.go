package providers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func copilotCfg(aliases map[string]string, enabled bool) *Config {
	return &Config{Providers: []ProviderConfig{
		{ProviderID: "copilot", Type: "copilot", ModelPrefix: "copilot", Enabled: enabled, ModelAliases: aliases},
	}}
}

func TestResolveCopilotAlias_Default(t *testing.T) {
	c := copilotCfg(nil, true)
	for _, ep := range []string{"/v1/responses", "/v1/chat/completions"} {
		if got := c.resolveCopilotAlias("codex-auto-review", ep); got != "copilot-gpt-5.6-luna" {
			t.Errorf("%s: got %q, want copilot-gpt-5.6-luna", ep, got)
		}
	}
	// Not applied to Anthropic Messages.
	if got := c.resolveCopilotAlias("codex-auto-review", "/v1/messages"); got != "" {
		t.Errorf("/v1/messages must not be aliased, got %q", got)
	}
	// Exact match only.
	if got := c.resolveCopilotAlias("codex-auto-review-2", "/v1/responses"); got != "" {
		t.Errorf("non-exact match aliased: %q", got)
	}
	if got := c.resolveCopilotAlias("gpt-5", "/v1/responses"); got != "" {
		t.Errorf("unrelated model aliased: %q", got)
	}
}

func TestResolveCopilotAlias_ExplicitReplacesDefault(t *testing.T) {
	c := copilotCfg(map[string]string{"my-alias": "gpt-5"}, true)
	if got := c.resolveCopilotAlias("my-alias", "/v1/responses"); got != "copilot-gpt-5" {
		t.Errorf("got %q", got)
	}
	// An explicit map replaces the built-in default.
	if got := c.resolveCopilotAlias("codex-auto-review", "/v1/responses"); got != "" {
		t.Errorf("default must be replaced by explicit map, got %q", got)
	}
}

func TestResolveCopilotAlias_EmptyMapDisables(t *testing.T) {
	c := copilotCfg(map[string]string{}, true)
	if got := c.resolveCopilotAlias("codex-auto-review", "/v1/responses"); got != "" {
		t.Errorf("empty map must disable aliases, got %q", got)
	}
}

func TestResolveCopilotAlias_DisabledOrMissingProvider(t *testing.T) {
	if got := copilotCfg(nil, false).resolveCopilotAlias("codex-auto-review", "/v1/responses"); got != "" {
		t.Errorf("disabled provider aliased: %q", got)
	}
	if got := (&Config{}).resolveCopilotAlias("codex-auto-review", "/v1/responses"); got != "" {
		t.Errorf("missing provider aliased: %q", got)
	}
}

func TestValidate_ModelAliases(t *testing.T) {
	cases := []struct {
		name    string
		aliases map[string]string
		wantErr bool
	}{
		{"ok", map[string]string{"a": "b"}, false},
		{"empty source", map[string]string{"": "b"}, true},
		{"empty target", map[string]string{"a": ""}, true},
		{"space source", map[string]string{" a": "b"}, true},
		{"space target", map[string]string{"a": "b "}, true},
		{"self", map[string]string{"a": "a"}, true},
		{"reserved prefix", map[string]string{"copilot-x": "b"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := copilotCfg(tc.aliases, true).Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidate_CopilotOnlyFields(t *testing.T) {
	cfg := &Config{Providers: []ProviderConfig{
		{ProviderID: "x", Type: "messages", ModelPrefix: "x", Auth: "api_key", APIKey: "k", ModelAliases: map[string]string{"a": "b"}},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("model_aliases on a non-copilot provider must be rejected")
	}
	cfg = &Config{Providers: []ProviderConfig{
		{ProviderID: "x", Type: "messages", ModelPrefix: "x", Auth: "api_key", APIKey: "k", ContextTier: "off"},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("context_tier on a non-copilot provider must be rejected")
	}
}

// End to end through Route: the alias is rewritten to the real model before
// the Copilot handler sees the body.
func TestRoute_AliasRewritesModelForCopilot(t *testing.T) {
	var gotModel string
	copilot := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &m)
		gotModel = m.Model
		w.WriteHeader(http.StatusOK)
	})
	c := copilotCfg(nil, true)
	c.copilotHandler = copilot
	c.copilotModels = map[string]copilotEndpoints{"gpt-5.6-luna": {supportsResponses: true}}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"codex-auto-review","input":"hi"}`))
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if gotModel != "gpt-5.6-luna" {
		t.Fatalf("copilot handler saw model %q, want gpt-5.6-luna", gotModel)
	}
}
