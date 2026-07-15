package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time assertions.
var _ Tool = (*RestartMainServiceTool)(nil)
var _ Tool = (*TailServiceLogsTool)(nil)

// RestartMainServiceTool triggers a graceful restart of the FTW service via
// POST /api/version/restart. Works whether the deploy is native systemd or
// docker-compose (the container has no systemctl).
type RestartMainServiceTool struct {
	apiBase string
	client  *http.Client
}

func NewRestartMainServiceTool(apiBase string) *RestartMainServiceTool {
	return &RestartMainServiceTool{
		apiBase: strings.TrimRight(apiBase, "/"),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *RestartMainServiceTool) Name() string { return "restart_main_service" }

func (t *RestartMainServiceTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "restart_main_service",
		Description: "Trigger a graceful restart of the FTW main service. The service exits and is brought back up by docker's restart policy (or systemd, depending on deploy). Drivers re-init from config.",
		InputSchema: map[string]any{"type": "object"},
	}
}

func (t *RestartMainServiceTool) Handle(ctx context.Context, _ map[string]any) (any, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", t.apiBase+"/api/version/restart", nil)
	if err != nil {
		return nil, fmt.Errorf("restart: build request: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("restart: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("restart: HTTP %d: %s", resp.StatusCode, body)
	}
	return map[string]any{"ok": true, "status": resp.StatusCode, "body": string(body)}, nil
}

// TailServiceLogsTool fetches recent in-process log entries via GET /api/logs.
// The endpoint accepts a "limit" query parameter (max 5000, default 500).
// The "since" parameter is accepted for schema compatibility but is not forwarded
// because the log ring is position-based, not time-based.
type TailServiceLogsTool struct {
	apiBase string
	client  *http.Client
}

func NewTailServiceLogsTool(apiBase string) *TailServiceLogsTool {
	return &TailServiceLogsTool{
		apiBase: strings.TrimRight(apiBase, "/"),
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (t *TailServiceLogsTool) Name() string { return "tail_service_logs" }

func (t *TailServiceLogsTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "tail_service_logs",
		Description: "Fetch recent log entries from the FTW in-process log ring via GET /api/logs.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"since": map[string]any{
					"type":        "string",
					"description": "Hint for how far back to fetch logs, e.g. \"30m\", \"1h\". Not currently applied server-side; use \"lines\" to control volume.",
				},
				"lines": map[string]any{
					"type":        "integer",
					"description": "Maximum number of log lines to return (default: 500, max: 5000)",
				},
			},
		},
	}
}

func (t *TailServiceLogsTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	limit := 500
	switch v := args["lines"].(type) {
	case int:
		limit = v
	case float64:
		limit = int(v)
	}
	if limit <= 0 {
		limit = 500
	}

	u, err := url.Parse(t.apiBase + "/api/logs")
	if err != nil {
		return nil, fmt.Errorf("tail_service_logs: parse url: %w", err)
	}
	q := u.Query()
	q.Set("limit", strconv.Itoa(limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("tail_service_logs: build request: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tail_service_logs: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("tail_service_logs: HTTP %d: %s", resp.StatusCode, body)
	}
	return map[string]any{"log": string(body)}, nil
}
