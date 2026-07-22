package providers

import (
	"os"
	"testing"
)

func TestValidate_DuplicatePrefix(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{ProviderID: "a", Type: "messages", ModelPrefix: "deepseek", Auth: "api_key", APIKey: "sk-a"},
			{ProviderID: "b", Type: "messages", ModelPrefix: "deepseek", Auth: "api_key", APIKey: "sk-b"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected duplicate prefix error")
	}
}

func TestValidate_ApiKeyAndOAuthExclusive(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{ProviderID: "x", Type: "messages", ModelPrefix: "x", Auth: "oauth", APIKey: "sk-x"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected mutual exclusion error")
	}
}

func TestValidate_ConvertResponsesOnCopilot(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{ProviderID: "copilot", Type: "copilot", ModelPrefix: "copilot", ConvertToResponses: true},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected convert_to_responses not supported for copilot")
	}
}

func TestValidate_InvalidProxyHost(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{ProviderID: "x", Type: "messages", ModelPrefix: "x", Auth: "api_key", APIKey: "k", ProxyHost: "://bad"},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid proxy_host to fail validation (no silent direct fallback)")
	}
}

func TestFindProvider(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{ProviderID: "deepseek", Type: "messages", ModelPrefix: "deepseek"},
			{ProviderID: "copilot", Type: "copilot", ModelPrefix: "copilot"},
		},
	}
	if p := cfg.FindProvider("deepseek-v4-pro"); p == nil || p.ProviderID != "deepseek" {
		t.Fatal("expected deepseek provider")
	}
	if p := cfg.FindProvider("copilot-gpt-5.4"); p == nil || p.ProviderID != "copilot" {
		t.Fatal("expected copilot provider")
	}
	if p := cfg.FindProvider("unknown-model"); p != nil {
		t.Fatal("expected nil for unknown model")
	}
}

func TestFindProvider_ExactPrefix(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{ProviderID: "a", Type: "messages", ModelPrefix: "ab"},
		},
	}
	if p := cfg.FindProvider("a-model"); p != nil {
		t.Fatal("a-model should not match prefix ab")
	}
	if p := cfg.FindProvider("ab-model"); p == nil {
		t.Fatal("ab-model should match prefix ab")
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/providers.json", nil)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadConfig_Valid(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/providers.json"
	data := `{"providers":[{"provider_id":"test","type":"messages","auth":"api_key","api_key":"sk-test","model_prefix":"test","enabled":true}]}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, nil)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(cfg.Providers))
	}
	p := cfg.Providers[0]
	if p.ProviderID != "test" {
		t.Errorf("expected provider_id 'test', got %q", p.ProviderID)
	}
	if p.client == nil {
		t.Errorf("expected client to be initialized by LoadConfig (K3 fix)")
	}
}
