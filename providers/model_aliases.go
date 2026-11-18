package providers

import (
	"fmt"
	"strings"
)

const (
	// defaultAliasSource/Target: Codex CLI's auto-review sends a pseudo model
	// id that no provider serves; it is mapped to a real Copilot model.
	defaultAliasSource = "codex-auto-review"
	defaultAliasTarget = "gpt-5.6-luna"
)

// defaultCopilotAliases is used when the copilot provider does not declare
// model_aliases at all. An explicit empty object ({}) disables aliases.
func defaultCopilotAliases() map[string]string {
	return map[string]string{defaultAliasSource: defaultAliasTarget}
}

// validateModelAliases checks a model_aliases map: both sides must be
// non-empty ids without surrounding whitespace, and an id cannot alias itself.
func validateModelAliases(providerID string, aliases map[string]string) error {
	for src, dst := range aliases {
		if src == "" || src != strings.TrimSpace(src) {
			return fmt.Errorf("provider %s: model_aliases source must be a non-empty model id without surrounding whitespace: %q", providerID, src)
		}
		if dst == "" || dst != strings.TrimSpace(dst) {
			return fmt.Errorf("provider %s: model_aliases target for %q must be a non-empty model id without surrounding whitespace", providerID, src)
		}
		if src == dst {
			return fmt.Errorf("provider %s: model_aliases %q cannot target itself", providerID, src)
		}
		if strings.HasPrefix(src, "copilot-") {
			return fmt.Errorf("provider %s: model_aliases source %q must not use the reserved 'copilot-' prefix", providerID, src)
		}
	}
	return nil
}

// resolveCopilotAlias returns the routable model id ("copilot-<target>") for an
// aliased client-visible model, or "" when there is no alias.
//
// Aliases apply only to the OpenAI-compatible endpoints (/v1/responses,
// /v1/chat/completions), where Codex-style pseudo models appear.
func (c *Config) resolveCopilotAlias(model, endpoint string) string {
	if endpoint != "/v1/responses" && endpoint != "/v1/chat/completions" {
		return ""
	}
	p := c.ByID("copilot")
	if p == nil || !p.Enabled || p.Type != "copilot" {
		return ""
	}
	aliases := p.ModelAliases
	if aliases == nil {
		aliases = defaultCopilotAliases()
	}
	target, ok := aliases[model]
	if !ok {
		return ""
	}
	return "copilot-" + target
}
