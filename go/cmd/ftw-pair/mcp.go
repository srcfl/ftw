package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool is the interface every ftw-pair tool must implement. Tools register
// themselves by passing a []Tool slice to StartMCP. The Tool interface uses
// map[string]any for arguments so higher-level tools can be wired up without
// importing the MCP SDK directly.
type Tool interface {
	Name() string
	// Schema returns the MCP tool descriptor. InputSchema must be non-nil and
	// have type "object" (per the MCP spec, enforced by Server.AddTool).
	Schema() *mcpsdk.Tool
	Handle(ctx context.Context, args map[string]any) (any, error)
}

// MCPConfig holds everything StartMCP needs to wire up the server.
type MCPConfig struct {
	Addr string
	// Stateless enables per-request sessions (no MCP initialize handshake
	// required). Useful for testing and simple local use where the client
	// doesn't maintain a persistent session.
	Stateless bool
	Session   *Session
	Audit     *Audit
	Tools     []Tool
}

// MCPServer wraps the net/http server and the listener so callers can
// discover the bound address (useful when Addr was "host:0").
type MCPServer struct {
	httpSrv *http.Server
	ln      net.Listener
}

// Addr returns the network address the server is listening on.
func (s *MCPServer) Addr() string { return s.ln.Addr().String() }

// Shutdown gracefully stops the HTTP server.
func (s *MCPServer) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

// StartMCP creates an MCP server, registers the supplied tools, attaches a
// /healthz endpoint, and starts serving. It returns as soon as the listener
// is bound; the server runs in background goroutines.
//
// The server shuts itself down cooperatively when either cfg.Session ends or
// ctx is cancelled — whichever comes first.
func StartMCP(ctx context.Context, cfg MCPConfig) (*MCPServer, error) {
	mcpSrv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "ftw-pair",
		Version: Version,
	}, nil)

	for _, t := range cfg.Tools {
		tool := t // capture loop variable
		// Use Server.AddTool (non-generic) so we can dispatch through the Tool
		// interface with map[string]any arguments rather than a concrete type.
		// We unmarshal the raw JSON arguments ourselves and delegate to the tool.
		mcpSrv.AddTool(tool.Schema(), func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			var args map[string]any
			if req.Params != nil && len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, fmt.Errorf("unmarshal args: %w", err)
				}
			}
			if args == nil {
				args = map[string]any{}
			}

			out, err := tool.Handle(ctx, args)
			ok := err == nil
			msg := "ok"
			if err != nil {
				msg = err.Error()
			}
			cfg.Audit.Append(AuditEvent{Tool: tool.Name(), Args: args, OutcomeOK: ok, OutcomeMsg: msg})

			if err != nil {
				res := &mcpsdk.CallToolResult{
					Content: []mcpsdk.Content{
						&mcpsdk.TextContent{Text: err.Error()},
					},
					IsError: true,
				}
				return res, nil
			}

			// Encode the output value as JSON text content.
			outJSON, jsonErr := json.Marshal(out)
			if jsonErr != nil {
				outJSON = []byte(fmt.Sprintf("%v", out))
			}
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{
					&mcpsdk.TextContent{Text: string(outJSON)},
				},
			}, nil
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	})
	// StreamableHTTPHandler serves MCP over HTTP (POST + SSE).
	var streamOpts *mcpsdk.StreamableHTTPOptions
	if cfg.Stateless {
		streamOpts = &mcpsdk.StreamableHTTPOptions{Stateless: true}
	}
	mux.Handle("/mcp", mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mcpSrv }, streamOpts))

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	httpSrv := &http.Server{Handler: mux}
	go httpSrv.Serve(ln) //nolint:errcheck

	// Cooperative shutdown: the server tears itself down when either the
	// session expires or the parent context is cancelled.
	go func() {
		select {
		case <-cfg.Session.Done():
		case <-ctx.Done():
		}
		_ = httpSrv.Shutdown(context.Background())
	}()

	return &MCPServer{httpSrv: httpSrv, ln: ln}, nil
}
