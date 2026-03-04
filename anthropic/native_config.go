package anthropic

import (
	"fmt"
	"os"
	"path/filepath"
)

// NativeConfig holds minimal configuration for the native Anthropic API handler.
type NativeConfig struct {
	APIBaseURL string
	Proxy      string
	cfg        *Config
}

// NewNativeConfig creates a NativeConfig with the given proxy and token directory.
func NewNativeConfig(proxy, tokenDir string) *NativeConfig {
	return &NativeConfig{
		APIBaseURL: "https://api.anthropic.com",
		Proxy:      proxy,
	}
}

// LoadOrAuthenticate loads stored credentials and ensures a valid token.
func (c *NativeConfig) LoadOrAuthenticate() error {
	cfg := &Config{
		APIBaseURL: c.APIBaseURL,
		Proxy:      c.Proxy,
	}
	if err := cfg.LoadOrAuthenticate(); err != nil {
		return fmt.Errorf("anthropic auth: %w", err)
	}
	c.cfg = cfg
	return nil
}

// GetAccessToken returns a valid access token.
func (c *NativeConfig) GetAccessToken() (string, error) {
	if c.cfg == nil {
		return "", fmt.Errorf("anthropic: not authenticated")
	}
	return c.cfg.GetAccessToken()
}

// GetAPIKey returns a valid API key.
func (c *NativeConfig) GetAPIKey() (string, error) {
	if c.cfg == nil {
		return "", fmt.Errorf("anthropic: not authenticated")
	}
	return c.cfg.GetAPIKey()
}

// IsClaudeAI reports whether using claude.ai flow.
func (c *NativeConfig) IsClaudeAI() bool {
	if c.cfg == nil {
		return false
	}
	return c.cfg.IsClaudeAI()
}

// ToNativeConfig returns itself.
func (c *NativeConfig) ToNativeConfig() *NativeConfig {
	return c
}

// NewConfigFromEnv creates a NativeConfig from environment variables.
func NewConfigFromEnv(proxy string) *NativeConfig {
	tokenDir := os.Getenv("COPILOT2API_TOKEN_DIR")
	if tokenDir == "" {
		home, _ := os.UserHomeDir()
		tokenDir = filepath.Join(home, ".config", "copilot2api")
	}
	return NewNativeConfig(proxy, tokenDir)
}
