package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/whtsky/copilot2api/internal/types"
)

// linguafrancaBridgePath returns the path to the linguafranca bridge script.
// Uses LINGUAFRANCA_BRIDGE env var if set, otherwise defaults to
// "scripts/linguafranca_bridge.py" relative to the working directory.
func linguafrancaBridgePath() string {
	if p := os.Getenv("LINGUAFRANCA_BRIDGE"); p != "" {
		return p
	}
	return "scripts/linguafranca_bridge.py"
}

// linguafrancaConvertRequest converts a Responses API JSON payload
// to Anthropic Messages API JSON via linguafranca (Python/Rust bridge).
func linguafrancaConvertRequest(responsesJSON []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", linguafrancaBridgePath(), "req")
	cmd.Stdin = bytes.NewReader(responsesJSON)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("linguafranca request: %w\nstderr: %s", err, stderr.String())
	}
	return out, nil
}

// linguafrancaConvertResponse converts an Anthropic non-streaming response
// to Responses API format via linguafranca.
func linguafrancaConvertResponse(anthropicResp []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", linguafrancaBridgePath(), "resp")
	cmd.Stdin = bytes.NewReader(anthropicResp)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("linguafranca response: %w\nstderr: %s", err, stderr.String())
	}
	return out, nil
}

// linguafrancaConvertStream reads Anthropic SSE events from body,
// pipes them through linguafranca for conversion to Responses SSE,
// and writes the converted events to w. Adds MCP namespace to
// function_call items for Codex routing.
func linguafrancaConvertStream(w io.Writer, body io.ReadCloser) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", linguafrancaBridgePath(), "stream")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("linguafranca stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("linguafranca stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("linguafranca start: %w", err)
	}

	// Ensure process is killed and waited on when we return.
	// cancel() kills the process via CommandContext; cmd.Wait reaps it.
	defer func() {
		cancel()
		if err := cmd.Wait(); err != nil {
			slog.Debug("linguafranca stream process exited", "error", err)
		}
	}()

	// Feed Anthropic SSE events to linguafranca in a goroutine.
	// Context cancellation will cause stdin.Close() to unblock on
	// the next write attempt when the client disconnects.
	go func() {
		defer stdin.Close()
		scanner := bufio.NewScanner(body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			dataStr := strings.TrimPrefix(line, "data: ")
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(dataStr), &event); err != nil {
				continue
			}
			eventJSON, err := json.Marshal(event)
			if err != nil {
				return
			}
			if _, err := stdin.Write(eventJSON); err != nil {
				return
			}
			if _, err := stdin.Write([]byte("\n")); err != nil {
				return
			}
		}
	}()

	// Read converted Responses SSE events and write to client.
	flusher, canFlush := w.(http.Flusher)
	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var event types.ResponseStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		// Post-process: attach MCP namespace for function_call items.
		// linguafranca doesn't know about MCP, so we add the namespace
		// field so Codex can route the call to the correct MCP server.
		if event.Item != nil && event.Item.Name != "" {
			if ns := types.ResolveMcpToolNamespace(event.Item.Name); ns != "" {
				event.Item.Namespace = ns
			}
		}

		data, _ := json.Marshal(event)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data); err != nil {
			return err
		}
		if canFlush {
			flusher.Flush()
		}
	}

	return scanner.Err()
}
