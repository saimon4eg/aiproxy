package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config holds OpenAI OAuth configuration.
type Config struct {
	Proxy    string
	credPath string

	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	idToken      string
	expiresAt    time.Time
}

// NewConfig builds a Config with the given proxy.
func NewConfig(proxy string) *Config {
	tokenDir := os.Getenv("COPILOT2API_TOKEN_DIR")
	if tokenDir == "" {
		home, _ := os.UserHomeDir()
		tokenDir = filepath.Join(home, ".config", "copilot2api")
	}
	return &Config{
		Proxy:    proxy,
		credPath: filepath.Join(tokenDir, "openai-credentials.json"),
	}
}

// LoadOrAuthenticate loads stored credentials and ensures a valid token.
func (c *Config) LoadOrAuthenticate() error {
	if err := c.loadFromDisk(); err == nil && c.hasUsableToken() {
		return nil
	}

	c.mu.RLock()
	rt := c.refreshToken
	c.mu.RUnlock()

	if rt != "" {
		client := makeAuthClient(c.Proxy)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tr, refreshErr := doRefreshWithContext(ctx, client, rt)
		if refreshErr == nil {
			c.storeTokens(tr.AccessToken, tr.RefreshToken, tr.IDToken)
			return nil
		}
		slog.Warn("openai: initial token refresh failed", "error", refreshErr)
	}

	return c.interactiveDeviceFlow()
}

// GetAccessToken returns the current access token, refreshing if needed.
func (c *Config) GetAccessToken() (string, error) {
	c.mu.RLock()
	tok := c.accessToken
	exp := c.expiresAt
	rt := c.refreshToken
	c.mu.RUnlock()

	if tok != "" && time.Until(exp) > 5*time.Minute {
		return tok, nil
	}

	if rt != "" {
		client := makeAuthClient(c.Proxy)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tr, err := doRefreshWithContext(ctx, client, rt)
		if err != nil {
			return "", fmt.Errorf("openai: token refresh failed: %w", err)
		}
		c.storeTokens(tr.AccessToken, tr.RefreshToken, tr.IDToken)
		c.mu.RLock()
		tok = c.accessToken
		c.mu.RUnlock()
		return tok, nil
	}

	return "", fmt.Errorf("openai: no valid access token; enable provider in providers.json and restart")
}

func (c *Config) hasUsableToken() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accessToken != "" && time.Until(c.expiresAt) > 5*time.Minute
}

func (c *Config) interactiveDeviceFlow() error {
	client := makeAuthClient(c.Proxy)

	fmt.Println()
	fmt.Println("=== OpenAI Device Authorization ===")

	dc, err := RequestDeviceCode(client)
	if err != nil {
		return fmt.Errorf("openai: failed to request device code: %w", err)
	}

	fmt.Printf("\n1. Open: %s\n", dc.VerificationURL)
	fmt.Printf("2. Enter code: %s\n", dc.UserCode)
	fmt.Printf("\nWaiting for authorization (timeout: 15 minutes)...\n")

	code, err := PollForToken(client, dc)
	if err != nil {
		return fmt.Errorf("openai: device auth failed: %w", err)
	}

	tr, err := ExchangeCode(client, code)
	if err != nil {
		return fmt.Errorf("openai: token exchange failed: %w", err)
	}

	c.storeTokens(tr.AccessToken, tr.RefreshToken, tr.IDToken)

	if claims, err := ParseJWTPayload(tr.IDToken); err == nil {
		email := ""
		if e, ok := claims["email"].(string); ok {
			email = e
		}
		fmt.Printf("✓ Authorized as %s\n", email)
	} else {
		fmt.Println("✓ Authorized")
	}

	return nil
}

func (c *Config) storeTokens(accessToken, refreshToken, idToken string) {
	now := time.Now()
	expiresAt := now.Add(1 * time.Hour)

	if claims, err := ParseJWTPayload(accessToken); err == nil {
		if exp, ok := claims["exp"].(float64); ok {
			expiresAt = time.Unix(int64(exp), 0)
		}
	}

	c.mu.Lock()
	c.accessToken = accessToken
	c.refreshToken = refreshToken
	c.idToken = idToken
	c.expiresAt = expiresAt
	c.mu.Unlock()

	creds := storedCreds{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		IDToken:      idToken,
		ExpiresAt:    expiresAt.UnixMilli(),
	}
	if err := c.saveToDisk(creds); err != nil {
		slog.Warn("openai: failed to save credentials", "error", err)
	}
}

func (c *Config) loadFromDisk() error {
	data, err := os.ReadFile(c.credPath)
	if err != nil {
		return err
	}
	var creds storedCreds
	if err := json.Unmarshal(data, &creds); err != nil {
		return err
	}
	c.mu.Lock()
	c.accessToken = creds.AccessToken
	c.refreshToken = creds.RefreshToken
	c.idToken = creds.IDToken
	c.expiresAt = time.UnixMilli(creds.ExpiresAt)
	c.mu.Unlock()
	return nil
}

func (c *Config) saveToDisk(creds storedCreds) error {
	if err := os.MkdirAll(filepath.Dir(c.credPath), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.credPath, data, 0600)
}

func doRefreshWithContext(ctx context.Context, client *http.Client, refreshToken string) (*tokenResp, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {defaultClientID},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", defaultIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("token refresh returned %d: %s", resp.StatusCode, string(body))
	}

	var tr tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("failed to parse refresh response: %w", err)
	}
	return &tr, nil
}
