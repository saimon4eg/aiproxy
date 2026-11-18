package providers

// Spawning the official Claude Code CLI as a request-scoped model client.
//
// Everything here exists to keep native from acting as an agent: tools,
// slash commands, settings sources and session persistence are all disabled at
// the command line, and the child's environment is a whitelist so no credential
// from the host can silently take over billing.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/whtsky/copilot2api/internal/upstream"
)

// installHints are searched when "claude" is not on PATH: a service started with
// a minimal environment still finds the binary.
var installHints = []string{
	"~/.local/bin", "~/.claude/local", "~/bin", "~/.npm-global/bin",
	"~/.bun/bin", "~/.volta/bin", "/opt/homebrew/bin", "/usr/local/bin",
}

const installHint = "Claude Code CLI not found. Install it with " +
	"`npm install -g @anthropic-ai/claude-code` and run `claude auth login`, " +
	"or point CLAUDE_CLI_PATH at the binary."

// childEnvPassthrough is the minimum needed for a sane process.
//
// Nothing credential-bearing and nothing network-routing-bearing is inherited:
// a whitelist is the only way to guarantee that a stray ANTHROPIC_API_KEY in the
// container (which outranks the subscription login and would move billing to the
// paid API with no error) or a host-wide HTTP_PROXY can never take over the
// child. The proxy is set explicitly from the provider's own proxy_host below.
//
// HOME is dropped on purpose: with CLAUDE_CONFIG_DIR set, native finds no ambient
// login to fall back to, which is what turns a dead token into a 401 instead of
// silently running on another account.
var childEnvPassthrough = []string{
	"PATH", "TMPDIR", "TEMP", "TMP", "LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "CLAUDE_CLI_TIMEOUT",
}

// childNoProxy keeps the loopback relay out of the proxy. Without it the CLI
// sends its own request to the admission relay through the proxy, which is both
// wrong and fatal: the relay only listens on 127.0.0.1.
const childNoProxy = "127.0.0.1,localhost,::1"

// claudeProc is a prepared child: argv, environment and the temp files it reads.
type claudeProc struct {
	command []string
	env     []string
	workdir string
	root    string // per-request temp dir, removed by cleanup
}

// sharedWorkdir is resolved once: the directory has to be the same for every child
// this process starts, so a per-call lookup could only ever disagree with itself.
var (
	sharedWorkdirOnce sync.Once
	sharedWorkdirPath string
)

// sharedClaudeWorkdir is the one cwd handed to every child, or "" when no directory
// can be made private enough.
//
// Native writes its cwd into the environment block it prepends to each request, so a
// per-request directory moves the prompt-cache prefix on every turn: the entry is
// written under one path and read under another, and every turn is charged the
// cache-creation rate instead of the read rate. One directory per OS user keeps that
// block byte-identical across requests.
func sharedClaudeWorkdir() string {
	sharedWorkdirOnce.Do(func() {
		path := filepath.Join(os.TempDir(), sharedDirName())
		// A parent others can write lets them rename our directory out from under
		// us; a sticky parent — the usual /tmp — does not.
		parent, err := os.Stat(filepath.Dir(path))
		if err != nil || !parent.IsDir() {
			return
		}
		if parent.Mode().Perm()&0o022 != 0 && parent.Mode()&os.ModeSticky == 0 {
			return
		}
		if !prepareSharedDir(path) {
			return
		}
		// Hermes prunes scratch directories idle for a day; keep ours out of that
		// sweep, since losing it would move the cwd again.
		now := time.Now()
		_ = os.Chtimes(path, now, now)
		sharedWorkdirPath = path
	})
	return sharedWorkdirPath
}

// resolveClaude finds the CLI binary.
func resolveClaude(configured string, env map[string]string) (string, error) {
	candidate := strings.TrimSpace(configured)
	if candidate == "" {
		candidate = strings.TrimSpace(env["CLAUDE_CLI_PATH"])
	}
	if candidate == "" {
		candidate = "claude"
	}
	if path, err := exec.LookPath(candidate); err == nil {
		return path, nil
	}
	if candidate != "claude" && strings.ContainsRune(candidate, os.PathSeparator) {
		return "", fmt.Errorf("%s: %s", candidate, installHint)
	}
	for _, dir := range installHints {
		expanded := dir
		if strings.HasPrefix(dir, "~/") {
			expanded = filepath.Join(os.Getenv("HOME"), strings.TrimPrefix(dir, "~/"))
		}
		full := filepath.Join(expanded, "claude")
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			return full, nil
		}
	}
	return "", fmt.Errorf("%s", installHint)
}

// buildChildEnv produces the CLI's environment from a whitelist.
//
// homeDir is a directory we own and keep empty: native resolves a missing HOME
// through /etc/passwd, so dropping the variable alone would still let it find a
// real ~/.claude and run on whatever account the machine was logged into.
//
// proxyHost is the provider's own proxy. It is applied here, not inherited, so
// every claude-cli provider keeps its own egress path no matter what the host or
// container has configured globally; NO_PROXY always covers loopback because the
// relay the child talks to only exists on 127.0.0.1.
func buildChildEnv(parent map[string]string, admissionURL, configDir, homeDir string, maxOutput int, proxyHost string) []string {
	env := make(map[string]string, len(parent)+8)
	for _, key := range childEnvPassthrough {
		if v, ok := parent[key]; ok && v != "" {
			env[key] = v
		}
	}
	// Windows needs a few more names to start at all; harmless elsewhere.
	if runtime.GOOS == "windows" {
		for _, key := range []string{"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
			if v, ok := parent[key]; ok && v != "" {
				env[key] = v
			}
		}
	}

	if admissionURL != "" {
		env["ANTHROPIC_BASE_URL"] = admissionURL
	}
	if configDir != "" {
		env["CLAUDE_CONFIG_DIR"] = configDir
	}
	if homeDir != "" {
		env["HOME"] = homeDir
	}
	env["CLAUDE_CODE_MAX_RETRIES"] = "0"
	env["DISABLE_AUTO_COMPACT"] = "1"
	env["DISABLE_COMPACT"] = "1"
	env["ENABLE_TOOL_SEARCH"] = "false"
	// Hermes owns budgets; native's reminder changes every request and breaks the
	// cached prefix.
	env["CLAUDE_CODE_TOTAL_TOKENS_REMINDER"] = "off"
	if maxOutput > 0 {
		env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] = fmt.Sprintf("%d", maxOutput)
	}
	// The provider's proxy governs the child's own egress (the OAuth refresh the
	// CLI may perform). Loopback stays out of it, or the relay hop would be sent
	// to the proxy and fail with a connection refused.
	if proxy := childProxyURL(proxyHost); proxy != "" {
		env["HTTP_PROXY"] = proxy
		env["HTTPS_PROXY"] = proxy
		env["http_proxy"] = proxy
		env["https_proxy"] = proxy
		env["NO_PROXY"] = childNoProxy
		env["no_proxy"] = childNoProxy
	}
	// The extra body travels through the private settings file, not the
	// environment: a full tool inventory overflows the per-string limit.

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

// childProxyURL normalises a provider's proxy_host into something the CLI can
// read from HTTP(S)_PROXY. An empty value means "no proxy", never "inherit one".
func childProxyURL(proxyHost string) string {
	raw := strings.TrimSpace(proxyHost)
	if raw == "" {
		return ""
	}
	parsed, err := upstream.ParseProxyURL(raw)
	if err != nil || parsed.Host == "" {
		slog.Warn("claude-cli: unusable proxy_host, the CLI will not be given a proxy",
			"proxy_host", raw, "error", err)
		return ""
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		// The CLI's HTTP stack has no SOCKS support. Saying so beats handing it a
		// URL it will silently ignore.
		slog.Warn("claude-cli: proxy scheme is not supported by the CLI, it will not be given a proxy",
			"proxy_host", raw, "scheme", scheme)
		return ""
	}
	return parsed.String()
}

// prepareClaudeProc writes the per-request files and assembles argv.
//
// Tools and the system prompt travel as files because the full inventory of a
// host toolset overflows both argv and the per-environment-string limit.
//
// The environment is deliberately not built here: the admission URL only exists
// after the relay is listening, and assembling the env twice invited the two
// copies to drift apart.
func prepareClaudeProc(parent map[string]string, configuredCmd, workdir string,
	plan *claudeRequest) (*claudeProc, error) {

	binary, err := resolveClaude(configuredCmd, parent)
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "claude-cli-")
	if err != nil {
		return nil, fmt.Errorf("create request directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }

	manifestPath := filepath.Join(root, "tools.json")
	manifest := plan.Manifest
	if manifest == nil {
		manifest = []manifestTool{}
	}
	encodedManifest, err := json.Marshal(manifest)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("encode tool manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, encodedManifest, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("write tool manifest: %w", err)
	}

	systemPath := filepath.Join(root, "system.md")
	if err := os.WriteFile(systemPath, []byte(plan.System), 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("write system prompt: %w", err)
	}

	settingsPath := filepath.Join(root, "settings.json")
	settings := map[string]any{"env": map[string]string{"CLAUDE_CODE_EXTRA_BODY": plan.ExtraBody}}
	encodedSettings, err := json.Marshal(settings)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, encodedSettings, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("write settings: %w", err)
	}

	helper, helperErr := inertMCPHelperPath()
	if helperErr != nil {
		cleanup()
		return nil, helperErr
	}
	mcpConfig, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"hermes": map[string]any{
				"command": helper.command,
				"args":    append(append([]string{}, helper.args...), manifestPath),
			},
		},
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("encode mcp config: %w", err)
	}

	argv := []string{
		"-p",
		"--model", plan.Model,
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--tools", "",
		"--system-prompt-file", systemPath,
		"--settings", settingsPath,
		"--setting-sources", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--max-turns", "1",
		"--permission-mode", "dontAsk",
		"--no-session-persistence",
		"--mcp-config", string(mcpConfig),
	}
	if plan.Effort != "" {
		argv = append(argv, "--effort", plan.Effort)
	}

	if workdir == "" {
		workdir = sharedClaudeWorkdir()
	}
	if workdir == "" {
		// No directory private enough to share: one per request keeps the request
		// working and costs cache hits, which is the cheaper failure.
		workdir = root
	}
	return &claudeProc{
		command: append([]string{binary}, argv...),
		workdir: workdir,
		root:    root,
	}, nil
}

// claudeStream owns the child process and its stdout reader.
type claudeStream struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	events  chan any
	cleanup func()
	once    sync.Once
	// done unblocks a reader stuck on a full events channel, so cmd.Wait is
	// always reached and the child is always reaped.
	done      chan struct{}
	closeOnce sync.Once
}

// eventError carries a stdout line that was not valid stream-json. Keeping the
// offending line is the whole diagnosis when a wrapper prints a banner.
type eventError struct {
	Line string
	Err  error
}

func (e eventError) Error() string {
	return fmt.Sprintf("invalid stream-json output: %q: %v", truncate(e.Line, 300), e.Err)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// startClaude spawns the child in its own process group so cancellation can take
// the whole tree down: the npm shim execs node, and node must not outlive the
// request.
func startClaude(ctx context.Context, proc *claudeProc) (*claudeStream, error) {
	cmd := exec.Command(proc.command[0], proc.command[1:]...)
	cmd.Env = proc.env
	cmd.Dir = proc.workdir
	cmd.Stderr = os.Stderr
	setProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	stream := &claudeStream{
		cmd:    cmd,
		stdin:  stdin,
		events: make(chan any, 64),
		done:   make(chan struct{}),
		cleanup: func() {
			_ = os.RemoveAll(proc.root)
		},
	}

	go func() {
		defer close(stream.events)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			var event map[string]any
			var emit any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				emit = eventError{Line: line, Err: err}
			} else {
				emit = event
			}
			select {
			case stream.events <- emit:
			case <-stream.done:
				// The request was abandoned: stop reading, but still reap.
				_ = cmd.Wait()
				return
			}
			if _, bad := emit.(eventError); bad {
				break
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case stream.events <- eventError{Line: "", Err: err}:
			case <-stream.done:
			}
		}
		_ = cmd.Wait()
	}()

	go func() {
		<-ctx.Done()
		stream.kill()
	}()

	return stream, nil
}

// send writes one frame and flushes, so native can acknowledge it immediately.
func (s *claudeStream) send(frame nativeFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if _, err := s.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}

// kill tears the process group down without waiting on a hung child.
func (s *claudeStream) kill() {
	s.once.Do(func() {
		if s.cmd.Process == nil {
			return
		}
		killProcessGroup(s.cmd)
	})
}

// close releases everything this request owned.
func (s *claudeStream) close() {
	s.kill()
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	s.closeOnce.Do(func() { close(s.done) })
	if s.cleanup != nil {
		s.cleanup()
	}
}

// next reads the next event, giving up after `idle` of silence.
//
// idle is a gap, not a budget: a long generation that keeps emitting is never
// cut off, which is what a total deadline used to do. A nil event with a nil
// error means the child exited.
func (s *claudeStream) next(idle time.Duration) (map[string]any, error) {
	if idle <= 0 {
		idle = defaultReadIdle
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	select {
	case raw, ok := <-s.events:
		if !ok {
			return nil, nil
		}
		switch value := raw.(type) {
		case eventError:
			return nil, value
		case map[string]any:
			return value, nil
		default:
			return nil, fmt.Errorf("unexpected stream event %T", raw)
		}
	case <-s.done:
		return nil, nil
	case <-timer.C:
		return nil, fmt.Errorf("claude request timed out after %s without output", idle)
	}
}

func setProcessGroup(cmd *exec.Cmd) {
	setOwnProcessGroup(cmd)
}

func killProcessGroup(cmd *exec.Cmd) {
	killOwnProcessGroup(cmd)
}
