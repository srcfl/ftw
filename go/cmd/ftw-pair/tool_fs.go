package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time assertions: all three types must satisfy the Tool interface.
var _ Tool = (*ReadFileTool)(nil)
var _ Tool = (*WriteFileTool)(nil)
var _ Tool = (*ListDirectoryTool)(nil)

// ReadFileTool reads a file that is within the allowed scope.
type ReadFileTool struct {
	scope *Scope
}

func NewReadFileTool(sc *Scope) *ReadFileTool { return &ReadFileTool{scope: sc} }

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "read_file",
		Description: "Read a file that is within the allowed scope (repo dir, state dir, or /tmp).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Absolute path to the file to read"},
			},
			"required": []string{"path"},
		},
	}
}

func (t *ReadFileTool) Handle(_ context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	abs, err := t.scope.Resolve(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read_file: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("read_file stat: %w", err)
	}
	return map[string]any{
		"path":    abs,
		"content": string(data),
		"size":    info.Size(),
	}, nil
}

// WriteFileTool writes content to a file within the allowed scope and records
// the before/after diff in the audit log.
type WriteFileTool struct {
	scope *Scope
	audit *Audit
}

func NewWriteFileTool(sc *Scope, a *Audit) *WriteFileTool {
	return &WriteFileTool{scope: sc, audit: a}
}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "write_file",
		Description: "Write content to a file within the allowed scope. Creates intermediate directories as needed.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Absolute path to the file to write"},
				"content": map[string]any{"type": "string", "description": "Content to write"},
			},
			"required": []string{"path", "content"},
		},
	}
}

func (t *WriteFileTool) Handle(_ context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)

	abs, err := t.scope.Resolve(path)
	if err != nil {
		return nil, err
	}

	// Read existing content for the diff — ignore errors (file may not exist yet).
	before := ""
	if existing, readErr := os.ReadFile(abs); readErr == nil {
		before = string(existing)
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("write_file mkdir: %w", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("write_file: %w", err)
	}

	t.audit.RecordFileWrite(abs, before, content)

	return map[string]any{"path": abs, "written": len(content)}, nil
}

// ListDirectoryTool lists entries in a directory within the allowed scope.
type ListDirectoryTool struct {
	scope *Scope
}

func NewListDirectoryTool(sc *Scope) *ListDirectoryTool { return &ListDirectoryTool{scope: sc} }

func (t *ListDirectoryTool) Name() string { return "list_directory" }

func (t *ListDirectoryTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "list_directory",
		Description: "List entries in a directory within the allowed scope.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Absolute path to the directory to list"},
			},
			"required": []string{"path"},
		},
	}
}

func (t *ListDirectoryTool) Handle(_ context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	abs, err := t.scope.Resolve(path)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("list_directory: %w", err)
	}

	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		info, infoErr := e.Info()
		size := int64(0)
		if infoErr == nil {
			size = info.Size()
		}
		out = append(out, map[string]any{
			"name": e.Name(),
			"dir":  e.IsDir(),
			"size": size,
		})
	}

	return map[string]any{"entries": out}, nil
}
