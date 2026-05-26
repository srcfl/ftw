package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time checks: both types must satisfy the Tool interface.
var _ Tool = (*NetworkScanTool)(nil)
var _ Tool = (*HTTPProbeTool)(nil)

// NetworkScanTool reads the kernel ARP table and returns a list of
// {ip, mac, device} entries for all neighbours currently visible on
// the local network.
type NetworkScanTool struct {
	arpReader func() (string, error)
}

func defaultArpReader() (string, error) {
	data, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return "", fmt.Errorf("read /proc/net/arp: %w", err)
	}
	return string(data), nil
}

func NewNetworkScanTool() *NetworkScanTool {
	return &NetworkScanTool{arpReader: defaultArpReader}
}

func (t *NetworkScanTool) Name() string { return "network_scan" }

func (t *NetworkScanTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "network_scan",
		Description: "Read the kernel ARP table and return all neighbours currently visible on the local network (IP, MAC, interface).",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (t *NetworkScanTool) Handle(_ context.Context, _ map[string]any) (any, error) {
	raw, err := t.arpReader()
	if err != nil {
		return nil, err
	}

	var entries []map[string]string
	lines := strings.Split(raw, "\n")
	// Skip the header line (index 0).
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		entries = append(entries, map[string]string{
			"ip":     fields[0],
			"mac":    fields[3],
			"device": fields[5],
		})
	}

	return map[string]any{"entries": entries}, nil
}

// HTTPProbeTool performs an HTTP GET against an arbitrary URL and returns
// the status code, response headers, and up to 4 KB of body.
type HTTPProbeTool struct {
	client *http.Client
}

func NewHTTPProbeTool() *HTTPProbeTool {
	return &HTTPProbeTool{
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func (t *HTTPProbeTool) Name() string { return "http_probe" }

func (t *HTTPProbeTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "http_probe",
		Description: "Probe an HTTP endpoint (GET). Returns status, headers, and up to 4 KB of body. Useful for detecting running services on the local network.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{"type": "string", "description": "URL to probe (e.g. http://192.168.1.10:8080/)"},
			},
			"required": []string{"url"},
		},
	}
}

func (t *HTTPProbeTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	url, _ := args["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("url is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("http_probe build request: %w", err)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_probe: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, fmt.Errorf("http_probe read body: %w", err)
	}

	headers := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		headers[k] = strings.Join(vs, ", ")
	}

	return map[string]any{
		"status":  resp.StatusCode,
		"headers": headers,
		"body":    string(body),
	}, nil
}
