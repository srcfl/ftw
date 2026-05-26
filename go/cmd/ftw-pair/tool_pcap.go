package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time assertion.
var _ Tool = (*PCapCaptureTool)(nil)

// PCapCaptureTool captures network traffic via tcpdump and writes a .pcap file.
type PCapCaptureTool struct {
	// run is injectable for testing; defaults to exec.CommandContext.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

// NewPCapCaptureTool returns a PCapCaptureTool wired to the real tcpdump binary.
func NewPCapCaptureTool() *PCapCaptureTool {
	return &PCapCaptureTool{
		run: func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		},
	}
}

func (t *PCapCaptureTool) Name() string { return "pcap_capture" }

func (t *PCapCaptureTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "pcap_capture",
		Description: "Capture network traffic on an interface using tcpdump and save to a .pcap file. Requires tcpdump to be installed and sufficient privileges.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"interface":  map[string]any{"type": "string", "description": "Network interface to capture on (e.g. eth0)"},
				"bpf_filter": map[string]any{"type": "string", "description": "BPF filter expression (e.g. \"tcp port 502\")"},
				"duration_s": map[string]any{"type": "number", "description": "Capture duration in seconds (1–60)"},
			},
			"required": []string{"interface", "bpf_filter", "duration_s"},
		},
	}
}

func (t *PCapCaptureTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	iface, _ := args["interface"].(string)
	if iface == "" {
		return nil, fmt.Errorf("pcap_capture: interface is required")
	}

	bpf, _ := args["bpf_filter"].(string)
	if bpf == "" {
		return nil, fmt.Errorf("pcap_capture: bpf_filter is required")
	}

	var durSecs int
	switch v := args["duration_s"].(type) {
	case float64:
		durSecs = int(v)
	case int:
		durSecs = v
	default:
		return nil, fmt.Errorf("pcap_capture: duration_s is required")
	}
	if durSecs < 1 {
		durSecs = 1
	}
	if durSecs > 60 {
		durSecs = 60
	}

	path := fmt.Sprintf("/tmp/ftw-pair-%d.pcap", time.Now().UnixNano())

	// tcpdump -i <iface> -w <path> -G <dur> -W 1 <filter>
	cmdArgs := []string{
		"tcpdump",
		"-i", iface,
		"-w", path,
		"-G", strconv.Itoa(durSecs),
		"-W", "1",
		bpf,
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(durSecs+5)*time.Second)
	defer cancel()

	if _, err := t.run(runCtx, cmdArgs...); err != nil {
		return nil, fmt.Errorf("pcap_capture: tcpdump failed: %w", err)
	}

	return map[string]any{"pcap_path": path}, nil
}
