package openai

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/whtsky/copilot2api/internal/upstream"
)

const (
	defaultIssuer     = "https://auth.openai.com"
	defaultClientID   = "codex-cli"
	deviceCodeTimeout = 15 * time.Minute
	defaultInterval   = 5 // seconds between polls
)

// DeviceCode holds the response from /deviceauth/usercode.
type DeviceCode struct {
	DeviceAuthID    string `json:"device_auth_id"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"-"`
	Interval        int    `json:"interval"`
}

// userCodeReq is the request body for /deviceauth/usercode.
type userCodeReq struct {
	ClientID string `json:"client_id"`
}

// userCodeResp is the response from /deviceauth/usercode.
type userCodeResp struct {
	DeviceAuthID string      `json:"device_auth_id"`
	UserCode     string      `json:"user_code"`
	Interval     json.Number `json:"interval"` // API returns string, not int
}

// tokenPollReq is the request body for /deviceauth/token polling.
type tokenPollReq struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
}

// codeSuccessResp is the response on successful device auth poll.
type codeSuccessResp struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

// tokenResp is the response from /oauth/token (code exchange or refresh).
type tokenResp struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// storedCreds is the on-disk format.
type storedCreds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix millis
}

// makeAuthClient creates an HTTP client that routes through the given proxy if set.
func makeAuthClient(proxyHost string) *http.Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if proxyHost != "" {
		if u, err := upstream.ParseProxyURL(proxyHost); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

// RequestDeviceCode requests a user code from the OpenAI device auth endpoint.
func RequestDeviceCode(client *http.Client) (*DeviceCode, error) {
	body, _ := json.Marshal(userCodeReq{ClientID: defaultClientID})

	resp, err := client.Post(
		defaultIssuer+"/api/accounts/deviceauth/usercode",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("deviceauth usercode request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("device code login is not enabled for this server")
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("deviceauth usercode returned %d: %s", resp.StatusCode, string(respBody))
	}

	var uc userCodeResp
	if err := json.NewDecoder(resp.Body).Decode(&uc); err != nil {
		return nil, fmt.Errorf("failed to parse usercode response: %w", err)
	}

	interval := defaultInterval
	if v, err := uc.Interval.Int64(); err == nil && v > 0 {
		interval = int(v)
	}

	return &DeviceCode{
		DeviceAuthID:    uc.DeviceAuthID,
		UserCode:        uc.UserCode,
		VerificationURL: defaultIssuer + "/codex/device",
		Interval:        interval,
	}, nil
}

// PollForToken polls the device auth token endpoint until a code is issued or timeout.
func PollForToken(client *http.Client, dc *DeviceCode) (*codeSuccessResp, error) {
	deadline := time.Now().Add(deviceCodeTimeout)
	pollBody, _ := json.Marshal(tokenPollReq{
		DeviceAuthID: dc.DeviceAuthID,
		UserCode:     dc.UserCode,
	})

	interval := dc.Interval
	if interval <= 0 {
		interval = int(defaultInterval)
	}

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("device auth timed out after %v", deviceCodeTimeout)
		}

		resp, err := client.Post(
			defaultIssuer+"/api/accounts/deviceauth/token",
			"application/json",
			bytes.NewReader(pollBody),
		)
		if err != nil {
			slog.Debug("deviceauth poll failed, retrying", "error", err)
			time.Sleep(time.Duration(interval) * time.Second)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			var code codeSuccessResp
			if err := json.NewDecoder(resp.Body).Decode(&code); err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("failed to parse token response: %w", err)
			}
			resp.Body.Close()
			return &code, nil
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			time.Sleep(time.Duration(interval) * time.Second)
			continue
		}

		return nil, fmt.Errorf("deviceauth token returned %d", resp.StatusCode)
	}
}

// ExchangeCode exchanges an authorization code for tokens.
func ExchangeCode(client *http.Client, codeResp *codeSuccessResp) (*tokenResp, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeResp.AuthorizationCode},
		"redirect_uri":  {defaultIssuer + "/deviceauth/callback"},
		"code_verifier": {codeResp.CodeVerifier},
		"client_id":     {defaultClientID},
	}
	return postToken(client, form)
}

// RefreshToken refreshes an access token using a refresh token.
func RefreshToken(client *http.Client, refreshToken string) (*tokenResp, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {defaultClientID},
	}
	return postToken(client, form)
}

func postToken(client *http.Client, form url.Values) (*tokenResp, error) {
	req, err := http.NewRequest("POST", defaultIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tr tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}
	return &tr, nil
}

// ParseJWTPayload extracts the payload from a JWT without verifying signature.
func ParseJWTPayload(jwt string) (map[string]interface{}, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}
	payload := parts[1]
	// Add padding.
	switch m := len(payload) % 4; m {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to decode JWT payload: %w", err)
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil, fmt.Errorf("failed to parse JWT claims: %w", err)
	}
	return claims, nil
}
