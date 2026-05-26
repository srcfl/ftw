// Package main — rest.go
//
// Local-HTTP tool surface, served alongside the MCP /mcp endpoint.
//
// The REST layer exists so a connecting agent (Claude Code, Codex, Gemini,
// anything else with Bash + curl) can drive the sidecar without us having
// to register an MCP server with their CLI. The friend runs ftw-connect,
// gets a local URL, and the agent's prompt teaches it to:
//
//	curl <URL>/tools                          # list available tools + schemas
//	curl -X POST <URL>/tools/<name> -d <json> # invoke a tool
//
// REST and MCP share the exact same Tool slice and Audit log — both routes
// dispatch through Tool.Handle and call Audit.Append on completion.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// registerRESTHandlers attaches /tools and /tools/<name> handlers to mux.
// Tools share the supplied audit log with the MCP path.
func registerRESTHandlers(mux *http.ServeMux, tools []Tool, audit *Audit) {
	index := make(map[string]Tool, len(tools))
	for _, t := range tools {
		index[t.Name()] = t
	}

	mux.HandleFunc("/tools", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		catalog := buildToolCatalog(tools)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(catalog)
	})

	mux.HandleFunc("/tools/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/tools/")
		if name == "" || strings.Contains(name, "/") {
			writeJSONError(w, http.StatusNotFound, "unknown tool")
			return
		}
		tool, ok := index[name]
		if !ok {
			writeJSONError(w, http.StatusNotFound, "unknown tool: "+name)
			return
		}
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "use POST with a JSON body")
			return
		}

		args, err := decodeArgs(r.Body)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}

		out, callErr := tool.Handle(r.Context(), args)
		ok2 := callErr == nil
		msg := "ok"
		if callErr != nil {
			msg = callErr.Error()
		}
		audit.Append(AuditEvent{Tool: tool.Name(), Args: args, OutcomeOK: ok2, OutcomeMsg: msg})

		w.Header().Set("Content-Type", "application/json")
		if callErr != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": callErr.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}

// toolEntry is the per-tool catalog shape returned by GET /tools.
type toolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

type toolCatalog struct {
	Tools []toolEntry `json:"tools"`
}

func buildToolCatalog(tools []Tool) toolCatalog {
	out := toolCatalog{Tools: make([]toolEntry, 0, len(tools))}
	for _, t := range tools {
		s := t.Schema()
		entry := toolEntry{Name: t.Name()}
		if s != nil {
			entry.Description = s.Description
			entry.InputSchema = s.InputSchema
		}
		out.Tools = append(out.Tools, entry)
	}
	return out
}

func decodeArgs(body io.Reader) (map[string]any, error) {
	buf, err := io.ReadAll(io.LimitReader(body, 1<<20)) // 1 MB cap
	if err != nil {
		return nil, err
	}
	args := map[string]any{}
	if len(buf) == 0 {
		return args, nil
	}
	if err := json.Unmarshal(buf, &args); err != nil {
		return nil, err
	}
	return args, nil
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
