package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time check: FtwAPITool must satisfy the Tool interface.
var _ Tool = (*FtwAPITool)(nil)

type FtwAPITool struct {
	baseURL string
	client  *http.Client
}

func NewFtwAPITool(baseURL string) *FtwAPITool {
	return &FtwAPITool{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{},
	}
}

func (t *FtwAPITool) Name() string { return "ftw_api" }

func (t *FtwAPITool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "ftw_api",
		Description: "Proxy an HTTP request to the running forty-two-watts service on localhost:8080. Path MUST start with /api/. Read docs/api.md for the available endpoints.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"method": map[string]any{"type": "string", "description": "HTTP method (GET/POST/PUT/DELETE)"},
				"path":   map[string]any{"type": "string", "description": "Path starting with /api/"},
				"body":   map[string]any{"type": "object", "description": "Optional JSON body for write methods"},
			},
			"required": []string{"method", "path"},
		},
	}
}

func (t *FtwAPITool) Handle(ctx context.Context, args map[string]any) (any, error) {
	method, _ := args["method"].(string)
	path, _ := args["path"].(string)
	if method == "" || path == "" {
		return nil, fmt.Errorf("method and path are required")
	}
	if !strings.HasPrefix(path, "/api/") {
		return nil, fmt.Errorf("path must start with /api/ (got %q)", path)
	}
	// The friend pair-flow must not reach owner-only control surfaces (pairing
	// control + owner-access credential management): they are gated server-side,
	// but refuse to forward them at all so a friend can't forge the owner's
	// pair-card state or probe owner-access. The genuine sidecar posts its own
	// status DIRECTLY to the Pi, not through this tool, so it is unaffected.
	if isOwnerOnlyPath(path) {
		return nil, fmt.Errorf("path %q is not permitted over a friend session", path)
	}

	var body io.Reader
	if b, ok := args["body"]; ok && b != nil {
		buf, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("encode body: %w", err)
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), t.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var parsed any
	if json.Unmarshal(respBody, &parsed) == nil {
		return map[string]any{"status": resp.StatusCode, "body": parsed}, nil
	}
	return map[string]any{"status": resp.StatusCode, "body": string(respBody)}, nil
}
