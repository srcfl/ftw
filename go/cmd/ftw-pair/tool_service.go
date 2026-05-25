package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time assertions.
var _ Tool = (*RestartMainServiceTool)(nil)
var _ Tool = (*TailServiceLogsTool)(nil)

// defaultExec runs a command and returns its combined output.
func defaultExec(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
}

// RestartMainServiceTool restarts the forty-two-watts systemd service.
type RestartMainServiceTool struct {
	run func(ctx context.Context, args ...string) ([]byte, error)
}

func NewRestartMainServiceTool() *RestartMainServiceTool {
	return &RestartMainServiceTool{run: defaultExec}
}

func (t *RestartMainServiceTool) Name() string { return "restart_main_service" }

func (t *RestartMainServiceTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "restart_main_service",
		Description: "Restart the forty-two-watts systemd service on the host.",
		InputSchema: map[string]any{
			"type": "object",
		},
	}
}

func (t *RestartMainServiceTool) Handle(ctx context.Context, _ map[string]any) (any, error) {
	out, err := t.run(ctx, "systemctl", "restart", "forty-two-watts")
	if err != nil {
		return nil, fmt.Errorf("restart_main_service: %w: %s", err, string(out))
	}
	return map[string]any{"output": string(out)}, nil
}

// TailServiceLogsTool reads recent journald logs for the forty-two-watts service.
type TailServiceLogsTool struct {
	run func(ctx context.Context, args ...string) ([]byte, error)
}

func NewTailServiceLogsTool() *TailServiceLogsTool {
	return &TailServiceLogsTool{run: defaultExec}
}

func (t *TailServiceLogsTool) Name() string { return "tail_service_logs" }

func (t *TailServiceLogsTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "tail_service_logs",
		Description: "Tail recent journald logs for the forty-two-watts service.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"since": map[string]any{
					"type":        "string",
					"description": "How far back to fetch logs, e.g. \"30m\", \"1h\" (default: \"30m\")",
				},
				"lines": map[string]any{
					"type":        "integer",
					"description": "Maximum number of log lines to return (default: 500)",
				},
			},
		},
	}
}

func (t *TailServiceLogsTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	since := "30m"
	if s, ok := args["since"].(string); ok && s != "" {
		since = s
	}

	lines := 500
	switch v := args["lines"].(type) {
	case int:
		lines = v
	case float64:
		lines = int(v)
	}
	if lines <= 0 {
		lines = 500
	}

	out, err := t.run(ctx,
		"journalctl",
		"-u", "forty-two-watts",
		"--since", since+" ago",
		"-n", strconv.Itoa(lines),
		"--no-pager",
	)
	if err != nil {
		return nil, fmt.Errorf("tail_service_logs: %w: %s", err, string(out))
	}
	return map[string]any{"log": string(out)}, nil
}
