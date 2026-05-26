package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time assertion.
var _ Tool = (*RunCommandTool)(nil)

// RunCommandTool executes a shell command inside the allowed scope.
type RunCommandTool struct {
	scope *Scope
}

func NewRunCommandTool(sc *Scope) *RunCommandTool { return &RunCommandTool{scope: sc} }

func (t *RunCommandTool) Name() string { return "run_command" }

func (t *RunCommandTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "run_command",
		Description: "Run a shell command in a working directory that is within the allowed scope.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cmd":       map[string]any{"type": "string", "description": "Shell command to execute via /bin/sh -c"},
				"workdir":   map[string]any{"type": "string", "description": "Absolute path to working directory (must be within scope)"},
				"timeout_s": map[string]any{"type": "integer", "description": "Timeout in seconds (default 30, max 600)"},
			},
			"required": []string{"cmd", "workdir"},
		},
	}
}

func (t *RunCommandTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	cmd, _ := args["cmd"].(string)
	workdir, _ := args["workdir"].(string)

	// Resolve workdir through scope — rejects anything outside allowed roots.
	absDir, err := t.scope.Resolve(workdir)
	if err != nil {
		return nil, fmt.Errorf("run_command: workdir out of scope: %w", err)
	}

	// Timeout: default 30s, clamped at 600s.
	timeoutSecs := 30
	if raw, ok := args["timeout_s"]; ok {
		switch v := raw.(type) {
		case int:
			timeoutSecs = v
		case float64:
			timeoutSecs = int(v)
		}
	}
	if timeoutSecs <= 0 {
		timeoutSecs = 30
	}
	if timeoutSecs > 600 {
		timeoutSecs = 600
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(runCtx, "/bin/sh", "-c", cmd)
	c.Dir = absDir
	c.Stdout = &stdout
	c.Stderr = &stderr

	exitCode := 0
	if runErr := c.Run(); runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			// Context timeout, binary not found, etc. — surface as error.
			return nil, fmt.Errorf("run_command: %w", runErr)
		}
	}

	return map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"exit_code": exitCode,
	}, nil
}
