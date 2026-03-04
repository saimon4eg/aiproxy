package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/whtsky/copilot2api/internal/upstream"
)

const (
	oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthToken    = "https://platform.claude.com/v1/oauth/token"

	// ── Platform / Console flow ──────────────────────────────────────────────
	platformAuthorize = "https://platform.claude.com/oauth/authorize"
	platformRedirect  = "https://platform.claude.com/oauth/code/callback"
	platformScopes    = "org:create_api_key user:profile user:inference"

	// ── Claude.ai subscription flow ──────────────────────────────────────────
	claudeAIAuthorize = "https://claude.ai/oauth/authorize"
	claudeAIScopes    = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

	// claudeAIOAuthCallbackPort is the localhost port for the OAuth redirect
	// callback server. In Docker this port must be mapped to the host
	// (docker compose run --service-ports). The port range 49152-65535 is
	// recommended for ephemeral use, but we choose a fixed port so that
	// docker-compose can map it predictably.
	claudeAIOAuthCallbackPort = 18921
)

// pkce generates a new PKCE code_verifier, code_challenge, and state.
type pkceParams struct {
	codeVerifier  string
	codeChallenge string
	state         string
}

func newPKCE() (*pkceParams, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("failed to generate code verifier: %w", err)
	}
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	h := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(h[:])

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("failed to generate state: %w", err)
	}

	return &pkceParams{
		codeVerifier:  codeVerifier,
		codeChallenge: codeChallenge,
		state:         fmt.Sprintf("%x", stateBytes),
	}, nil
}

// ── Platform (Console / API billing) OAuth flow ──────────────────────────────

// RunPKCEFlow runs an interactive PKCE OAuth2 flow for the Anthropic Console
// (API billing) and stores the resulting tokens + API key.
func (c *Config) RunPKCEFlow() error {
	pkce, err := newPKCE()
	if err != nil {
		return err
	}

	params := url.Values{
		"client_id":             {oauthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {platformRedirect},
		"scope":                 {platformScopes},
		"code_challenge":        {pkce.codeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {pkce.state},
	}
	authURL := platformAuthorize + "?" + params.Encode()

	fmt.Printf("\n🔐 Anthropic Console OAuth Required\n")
	fmt.Printf("Open this URL in your browser:\n\n  %s\n\n", authURL)
	fmt.Printf("After authorizing, paste the code shown on the page and press Enter:\n")

	reader := bufio.NewReader(os.Stdin)
	code, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read authorization code: %w", err)
	}
	code = strings.TrimSpace(code)
	if idx := strings.Index(code, "#"); idx != -1 {
		code = code[:idx]
	}
	if code == "" {
		return fmt.Errorf("authorization code is empty")
	}

	return c.finishPlatformAuth(code, pkce)
}

func (c *Config) finishPlatformAuth(code string, pkce *pkceParams) error {
	if err := c.exchangeCode(code, pkce.codeVerifier, pkce.state, platformRedirect); err != nil {
		return err
	}
	if err := c.createAPIKey(); err != nil {
		slog.Warn("anthropic: failed to create API key, will retry on first request", "error", err)
	} else {
		fmt.Printf("\n✅ Anthropic Console authentication successful!\n\n")
	}
	return nil
}

// ── Claude.ai subscription OAuth flow ────────────────────────────────────────

// RunClaudeAIPKCEFlow runs an interactive PKCE OAuth2 flow for Claude.ai
// (Claude Pro/Max/Code Pro subscription).
//
// Instead of a local callback server (which is fragile inside Docker), the user
// copies the full redirect URL from their browser's address bar and pastes it
// into the terminal. The code and state are parsed from the URL.
func (c *Config) RunClaudeAIPKCEFlow() error {
	pkce, err := newPKCE()
	if err != nil {
		return err
	}

	// Use a localhost redirect — the browser will navigate there but won't
	// connect (port is not mapped). The user copies the URL from the address bar.
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", claudeAIOAuthCallbackPort)

	params := url.Values{
		"code":                  {"true"},
		"client_id":             {oauthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {claudeAIScopes},
		"code_challenge":        {pkce.codeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {pkce.state},
	}
	authURL := claudeAIAuthorize + "?" + params.Encode()

	fmt.Printf("\n🔐 Claude.ai Subscription OAuth Required\n")
	fmt.Printf("Open this URL in your browser:\n\n  %s\n\n", authURL)
	fmt.Printf("After authorizing, your browser will try to open a localhost page and fail.\n")
	fmt.Printf("Copy the FULL URL from your browser's address bar and paste it here:\n")

	reader := bufio.NewReader(os.Stdin)
	rawURL, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read callback URL: %w", err)
	}
	rawURL = strings.TrimSpace(rawURL)

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("failed to parse callback URL: %w", err)
	}

	q := parsed.Query()
	code := q.Get("code")
	gotState := q.Get("state")
	gotError := q.Get("error")

	if gotError != "" {
		return fmt.Errorf("authorization error: %s — %s", gotError, q.Get("error_description"))
	}
	if code == "" {
		return fmt.Errorf("no authorization code found in URL; did you paste the full callback URL?")
	}
	if gotState != pkce.state {
		return fmt.Errorf("state mismatch: expected %s, got %s — possible CSRF attack", pkce.state, gotState)
	}

	if err := c.exchangeCode(code, pkce.codeVerifier, pkce.state, redirectURI); err != nil {
		return err
	}

	fmt.Printf("\n✅ Claude.ai authentication successful!\n\n")
	return nil
}

// ── Token helpers (shared by both flows) ────────────────────────────────────

func (c *Config) exchangeCode(code, codeVerifier, state, redirectURI string) error {
	return c.postToken(map[string]interface{}{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
		"client_id":     oauthClientID,
		"code_verifier": codeVerifier,
		"state":         state,
	})
}

func (c *Config) doRefresh(refreshToken string) error {
	return c.postToken(map[string]interface{}{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     oauthClientID,
	})
}

// doRefreshWithContext refreshes the token with a context for timeout control.
func (c *Config) doRefreshWithContext(ctx context.Context, refreshToken string) error {
	return c.postTokenWithContext(ctx, map[string]interface{}{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     oauthClientID,
	})
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"` // Unix ms
	TokenType    string `json:"token_type"`
}

func (c *Config) postToken(body map[string]interface{}) error {
	return c.postTokenWithContext(context.Background(), body)
}

func (c *Config) postTokenWithContext(ctx context.Context, body map[string]interface{}) error {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if c.Proxy != "" {
		if proxyURL, err := upstream.ParseProxyURL(c.Proxy); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthToken, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var tr tokenResponse
	if err := json.Unmarshal(respBody, &tr); err != nil {
		return fmt.Errorf("failed to parse token response: %w", err)
	}

	var expiresAtMs int64
	if tr.ExpiresAt > 0 {
		expiresAtMs = tr.ExpiresAt
	} else if tr.ExpiresIn > 0 {
		expiresAtMs = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).UnixMilli()
	} else {
		expiresAtMs = time.Now().Add(time.Hour).UnixMilli()
	}

	slog.Info("anthropic token obtained", "expires_at", time.UnixMilli(expiresAtMs))

	if err := c.storeTokens(tr.AccessToken, tr.RefreshToken, expiresAtMs); err != nil {
		return err
	}

	return nil
}

// ── API key creation (platform/console flow only) ────────────────────────────

// createAPIKey uses the current OAuth access token to create a real Anthropic
// API key (sk-ant-api03-...) via the Claude Code OAuth API key endpoint.
// Only meaning for the platform/console flow; claude.ai uses the OAuth token directly.
func (c *Config) createAPIKey() error {
	c.tokenMu.RLock()
	accessToken := c.accessToken
	c.tokenMu.RUnlock()
	if accessToken == "" {
		return fmt.Errorf("no access token available")
	}

	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if c.Proxy != "" {
		if proxyURL, err := upstream.ParseProxyURL(c.Proxy); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport}

	const apiKeyURL = "https://api.anthropic.com/api/oauth/claude_cli/create_api_key"
	req, err := http.NewRequest(http.MethodPost, apiKeyURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create API key request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("API key creation request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("API key endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var keyResp struct {
		RawKey string `json:"raw_key"`
	}
	if err := json.Unmarshal(respBody, &keyResp); err != nil {
		return fmt.Errorf("failed to parse API key response: %w", err)
	}

	key := keyResp.RawKey
	if key == "" {
		return fmt.Errorf("API key response contains no raw_key field: %s", strings.TrimSpace(string(respBody)))
	}

	slog.Info("anthropic API key created")
	return c.storeAPIKey(key)
}
