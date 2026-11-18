package providers

// claude-cli provider: login directory, request type alias, model catalog.

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/whtsky/copilot2api/anthropic"
)

// anthropicMessagesRequest is the client's request shape; the package-level alias
// keeps the handler readable.
type anthropicMessagesRequest = anthropic.AnthropicMessagesRequest

// loginHint is what the operator sees when the CLI has never logged in.
const loginHint = "Claude Code is not logged in. Run once, with a terminal:\n" +
	"    docker compose run --rm aiproxy\n" +
	"then complete the Claude login it prints."

// claudeConfigDir resolves where the CLI keeps its own login.
//
// This directory must NOT contain a copy of the host's login, and HOME is not
// passed to the child: with nothing to fall back to, a dead or missing token
// fails loudly instead of silently running on whatever account the machine was
// logged into.
func (p *ProviderConfig) claudeConfigDir(env map[string]string) string {
	if configured := strings.TrimSpace(p.ClaudeConfigDir); configured != "" {
		return configured
	}
	if fromEnv := strings.TrimSpace(env["CLAUDE_CONFIG_DIR"]); fromEnv != "" {
		return fromEnv
	}
	base := strings.TrimSpace(env["COPILOT2API_TOKEN_DIR"])
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".config", "copilot2api")
		}
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "claude")
}

// EnsureClaudeLoginDir creates the login directory if it does not exist yet, so
// the first interactive run has somewhere to write.
func EnsureClaudeLoginDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("claude login directory is not configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create claude login directory %s: %w", dir, err)
	}
	return nil
}

// CliLoginPresent reports whether the CLI holds a login in dir.
func CliLoginPresent(dir string) bool {
	if dir == "" {
		return false
	}
	for _, name := range []string{".credentials.json", "credentials.json"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// ClaudeLoginDir is the exported view of the login directory, for startup wiring.
func (p *ProviderConfig) ClaudeLoginDir(env map[string]string) string {
	return p.claudeConfigDir(env)
}

// claudeHomeDir is the empty HOME handed to the child. It lives next to the
// login directory so auth lookups that ignore CLAUDE_CONFIG_DIR still land in a
// directory we own and keep empty.
func claudeHomeDir(configDir string) string {
	if configDir == "" {
		return ""
	}
	home := configDir + "-home"
	_ = os.MkdirAll(home, 0o700)
	return home
}

// EnvMap converts a KEY=VALUE slice into a map, for startup wiring in main.
func EnvMap(entries []string) map[string]string {
	return envMap(entries)
}

// EnsureClaudeCLI makes sure every enabled claude-cli provider has a native
// login, running `claude auth login` inline when a terminal is attached.
//
// The operator-facing procedure does not change: `docker compose run --rm
// aiproxy` with no credentials starts the login; a headless start refuses with
// the same instruction.
func (c *Config) EnsureClaudeCLI(env map[string]string) error {
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Type != "claude-cli" {
			continue
		}
		dir := p.claudeConfigDir(env)
		if dir == "" {
			return fmt.Errorf("provider %s: claude login directory is not configured", p.ProviderID)
		}
		if err := EnsureClaudeLoginDir(dir); err != nil {
			return err
		}
		if CliLoginPresent(dir) {
			slog.Info("claude cli login present", "provider", p.ProviderID, "dir", dir)
			continue
		}
		if !stdinIsTerminal() {
			return fmt.Errorf("provider %s: %s", p.ProviderID, loginHint)
		}
		if err := runClaudeLogin(p, env, dir); err != nil {
			return fmt.Errorf("provider %s: %w", p.ProviderID, err)
		}
		if !CliLoginPresent(dir) {
			return fmt.Errorf("provider %s: login produced no credentials in %s", p.ProviderID, dir)
		}
	}
	return nil
}

// runClaudeLogin runs the CLI's own login with the terminal attached. The
// credential never passes through aiproxy: the CLI writes it into dir itself.
func runClaudeLogin(p *ProviderConfig, env map[string]string, dir string) error {
	binary, err := resolveClaude(p.Command, env)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("Claude Code login required (Claude Pro / Max subscription)")
	fmt.Println()
	cmd := exec.Command(binary, "auth", "login")
	childEnv := buildChildEnv(env, "", dir, claudeHomeDir(dir), 0, p.ProxyHost)
	cmd.Env = childEnv
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
