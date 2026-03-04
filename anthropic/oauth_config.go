package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Config holds configuration for the native Anthropic API client.
type Config struct {
	APIBaseURL   string
	Proxy        string // raw proxy string (e.g. "host.docker.internal:2080"); protocol auto-prepended
	flowType     string // "platform" or "claudeai"
	tokenMu      sync.RWMutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	apiKey       string // sk-ant-api03-... — only used by platform flow
	credPath     string
}

// storedCredentials is the on-disk format for Anthropic OAuth tokens.
type storedCredentials struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresAt        int64  `json:"expires_at"` // Unix milliseconds
	APIKey           string `json:"api_key,omitempty"`
	FlowType         string `json:"flow_type,omitempty"` // "platform" or "claudeai"
	SubscriptionType string `json:"subscription_type,omitempty"`
}

// IsClaudeAI reports whether the config uses the claude.ai subscription flow.
func (c *Config) IsClaudeAI() bool {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.flowType == "claudeai"
}

// IsPlatform reports whether the config uses the platform/console flow.
func (c *Config) IsPlatform() bool {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.flowType == "platform"
}

// LoadOrAuthenticate loads stored credentials from disk and ensures a valid
// token is available.
//
// When stored credentials exist but the token is expired, it attempts a fast
// refresh first. If that fails, a background goroutine retries periodically
// (10/20/40/80 s) regardless of TTY — the proxy may simply not be ready yet.
// Interactive OAuth is only triggered when there are NO stored credentials at
// all AND stdin is a terminal (first-time setup).
//
// GetAccessToken also attempts a refresh on the first real request, so the
// handler can recover even if all background retries are exhausted.
func (c *Config) LoadOrAuthenticate() error {
	loaded := false
	if err := c.loadFromDisk(); err == nil && c.hasUsableToken() {
		loaded = true
	}

	if !loaded {
		c.tokenMu.RLock()
		rt := c.refreshToken
		c.tokenMu.RUnlock()
		if rt != "" {
			// Fast attempt at startup — proxy may not be ready yet.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.doRefreshWithContext(ctx, rt)
			cancel()
			if err == nil {
				loaded = true
			} else {
				slog.Warn("anthropic: initial token refresh failed, will retry in background", "error", err)
			}
		}
	}

	if !loaded {
		// Credentials exist on disk (refresh token) but initial refresh
		// failed. Start background retry regardless of TTY — the proxy
		// may become available shortly. GetAccessToken also retries on
		// first real request. Only go interactive when there are NO
		// stored credentials at all (first-time setup).
		if rt := c.getRefreshToken(); rt != "" {
			go c.backgroundRefresh(rt)
			return fmt.Errorf(
				"anthropic: token refresh failed, retrying in background")
		}
		if isStdinTerminal() {
			return c.interactiveAuth()
		}
		return fmt.Errorf(
			"anthropic: no valid credentials and stdin is not a terminal; "+
				"run 'docker compose run aiproxy' once to complete OAuth, "+
				"or place credentials at %s", c.credPath)
	}

	// Platform flow: ensure we have an API key.
	c.tokenMu.RLock()
	isPlatform := c.flowType == "platform"
	hasAPIKey := c.apiKey != ""
	c.tokenMu.RUnlock()
	if isPlatform && !hasAPIKey {
		if err := c.createAPIKey(); err != nil {
			slog.Warn("anthropic: failed to create API key on startup, will retry on first request", "error", err)
		}
	}

	return nil
}

func (c *Config) getRefreshToken() string {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.refreshToken
}

// backgroundRefresh retries token refresh periodically after a failed startup
// attempt, to recover when the proxy becomes available.
func (c *Config) backgroundRefresh(refreshToken string) {
	delays := []time.Duration{10, 20, 40, 80} // seconds, total ~2.5 min
	for i, d := range delays {
		time.Sleep(d * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := c.doRefreshWithContext(ctx, refreshToken)
		cancel()
		if err == nil {
			slog.Info("anthropic: background token refresh succeeded", "attempt", i+1)
			return
		}
		slog.Debug("anthropic: background token refresh failed", "attempt", i+1, "error", err)
	}
	slog.Warn("anthropic: background token refresh exhausted retries, will try on first request")
}

func (c *Config) interactiveAuth() error {
	fmt.Println()
	fmt.Println("🔐 Claude.ai Subscription OAuth Required")
	fmt.Println("   (Claude Pro / Max / Code Pro subscription)")
	fmt.Println()
	c.setFlowType("claudeai")
	if err := c.RunClaudeAIPKCEFlow(); err != nil {
		return err
	}
	return nil
}

// GetAccessToken returns the current access token, refreshing if needed.
func (c *Config) GetAccessToken() (string, error) {
	c.tokenMu.RLock()
	tok := c.accessToken
	exp := c.expiresAt
	rt := c.refreshToken
	c.tokenMu.RUnlock()

	if tok != "" && time.Until(exp) > 5*time.Minute {
		return tok, nil
	}
	if rt != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.doRefreshWithContext(ctx, rt); err == nil {
			c.tokenMu.RLock()
			tok = c.accessToken
			c.tokenMu.RUnlock()
			return tok, nil
		}
	}
	return "", fmt.Errorf("anthropic: no valid access token; enable provider in providers.json and restart")
}

func (c *Config) hasUsableToken() bool {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.accessToken != "" && time.Until(c.expiresAt) > 5*time.Minute
}

func (c *Config) setFlowType(ft string) {
	c.tokenMu.Lock()
	c.flowType = ft
	c.tokenMu.Unlock()
}

func (c *Config) loadFromDisk() error {
	data, err := os.ReadFile(c.credPath)
	if err != nil {
		return err
	}
	var creds storedCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return err
	}
	c.tokenMu.Lock()
	c.accessToken = creds.AccessToken
	c.refreshToken = creds.RefreshToken
	c.expiresAt = time.UnixMilli(creds.ExpiresAt)
	c.apiKey = creds.APIKey
	// Backwards-compat: old credentials without flow_type default to "platform".
	if creds.FlowType == "" {
		c.flowType = "platform"
	} else {
		c.flowType = creds.FlowType
	}
	c.tokenMu.Unlock()
	return nil
}

func (c *Config) saveToDisk(creds storedCredentials) error {
	if err := os.MkdirAll(filepath.Dir(c.credPath), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.credPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, c.credPath)
}

func (c *Config) storeTokens(accessToken, refreshToken string, expiresAtMs int64) error {
	exp := time.UnixMilli(expiresAtMs)
	c.tokenMu.Lock()
	ft := c.flowType
	c.tokenMu.Unlock()
	creds := storedCredentials{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAtMs,
		FlowType:     ft,
	}
	c.tokenMu.Lock()
	c.accessToken = accessToken
	c.refreshToken = refreshToken
	c.expiresAt = exp
	c.tokenMu.Unlock()
	return c.saveToDisk(creds)
}

// GetAPIKey returns the API key for Anthropic API calls.
// Only meaningful for the platform/console flow.
// For claude.ai flow, use GetAccessToken() directly.
func (c *Config) GetAPIKey() (string, error) {
	c.tokenMu.RLock()
	key := c.apiKey
	tok := c.accessToken
	ft := c.flowType
	c.tokenMu.RUnlock()

	if key != "" {
		return key, nil
	}
	if ft == "claudeai" {
		return "", fmt.Errorf("anthropic: API key not available for claude.ai flow; use access token directly")
	}
	if tok == "" {
		return "", fmt.Errorf("anthropic: no access token available to create API key")
	}

	if err := c.createAPIKey(); err != nil {
		return "", err
	}

	c.tokenMu.RLock()
	key = c.apiKey
	c.tokenMu.RUnlock()
	return key, nil
}

// storeAPIKey saves the API key both in memory and on disk.
func (c *Config) storeAPIKey(key string) error {
	c.tokenMu.Lock()
	c.apiKey = key
	creds := storedCredentials{
		AccessToken:  c.accessToken,
		RefreshToken: c.refreshToken,
		ExpiresAt:    c.expiresAt.UnixMilli(),
		APIKey:       key,
		FlowType:     c.flowType,
	}
	c.tokenMu.Unlock()
	return c.saveToDisk(creds)
}

// isStdinTerminal reports whether stdin is connected to a terminal (TTY).
func isStdinTerminal() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}
