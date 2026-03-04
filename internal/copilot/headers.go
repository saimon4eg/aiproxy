package copilot

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Exported constants for User-Agent and version headers.
// Values match the JetBrains Copilot plugin's inference agent
// (copilot-language-server), captured from real traffic.
const (
	CopilotUserAgent = "copilot/1.0.63 (client/copilot-intellij linux v22.22.3) term/unknown"
	EditorVersion    = "copilot/1.0.63"
)

// integrationID and apiVersion identify the client to Copilot upstream.
// The integrator + API version gate model access and capabilities (e.g. 1M
// context). Defaults match the JetBrains Copilot plugin's inference agent
// (copilot-language-server); both configurable via providers.json.
var (
	integrationID = "copilot-developer-cli"
	apiVersion    = "2026-06-01"
)

// SetIntegrationID overrides the Copilot-Integration-Id sent upstream.
// Empty value keeps the current default. Call once at startup.
func SetIntegrationID(id string) {
	if id != "" {
		integrationID = id
	}
}

// SetAPIVersion overrides the X-Github-Api-Version sent upstream.
// Empty value keeps the current default. Call once at startup.
func SetAPIVersion(v string) {
	if v != "" {
		apiVersion = v
	}
}

// AddHeaders adds required Copilot headers to the request. The set mirrors the
// JetBrains Copilot inference agent exactly (copilot-developer-cli): no
// Editor-Plugin-Version, no Openai-Intent; X-Initiator marks user-initiated.
func AddHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", CopilotUserAgent)
	req.Header.Set("Editor-Version", EditorVersion)
	req.Header.Set("Copilot-Integration-Id", integrationID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Github-Api-Version", apiVersion)

	if req.Header.Get("X-Initiator") == "" {
		req.Header.Set("X-Initiator", "user")
	}
	// Generate request ID if not present
	if req.Header.Get("X-Request-Id") == "" {
		req.Header.Set("X-Request-Id", GenerateRequestID())
	}
}

// GenerateRequestID generates a unique request ID using crypto/rand
func GenerateRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		slog.Error("crypto/rand.Read failed", "error", err)
		return fmt.Sprintf("req_fallback_%d", time.Now().UnixNano())
	}
	return "req_" + hex.EncodeToString(b)
}
