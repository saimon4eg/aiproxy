package providers

// Inert MCP inventory.
//
// The model must SEE the host's tools to call them by name, but native must
// never execute one: Hermes owns the loop. So the CLI is given an MCP server
// that advertises the full inventory and denies every call. The server is this
// same binary re-invoked as a subcommand, so the container needs no Python and
// no extra executable.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// inertMCPHelper is how to launch the inventory server.
type inertMCPHelper struct {
	command string
	args    []string
}

// inertMCPHelperPath points at the running binary.
func inertMCPHelperPath() (inertMCPHelper, error) {
	exe, err := os.Executable()
	if err != nil {
		return inertMCPHelper{}, fmt.Errorf("locate own executable for the inert MCP server: %w", err)
	}
	return inertMCPHelper{command: exe, args: []string{"--inert-mcp"}}, nil
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
}

// RunInertMCP serves the tool inventory read from manifestPath over stdio until
// stdin closes. Only inventory methods are answered; anything executable is
// denied with an explicit error so the model learns the tool is not its to run.
func RunInertMCP(manifestPath string) error {
	var manifest []manifestTool
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read tool manifest: %w", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse tool manifest: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue // notification, no response expected
		}

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "hermes-inert-inventory", "version": "1"},
			}
		case "tools/list":
			tools := manifest
			if tools == nil {
				tools = []manifestTool{}
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			result = map[string]any{
				"isError": true,
				"content": []map[string]any{{
					"type": "text",
					"text": "Denied: native tools are inert; only the host executes tool calls.",
				}},
			}
		default:
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found: " + req.Method},
			})
			continue
		}

		if err := enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		}); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	return scanner.Err()
}
