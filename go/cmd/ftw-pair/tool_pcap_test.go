package main

import (
	"context"
	"strings"
	"testing"
)

func TestPCapCallsTcpdump(t *testing.T) {
	var got []string
	tool := &PCapCaptureTool{run: func(ctx context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte(""), nil
	}}
	out, err := tool.Handle(context.Background(), map[string]any{
		"interface":  "eth0",
		"bpf_filter": "tcp port 502",
		"duration_s": float64(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(got, " ")
	if !strings.Contains(cmd, "tcpdump") || !strings.Contains(cmd, "tcp port 502") {
		t.Fatalf("expected tcpdump call, got: %s", cmd)
	}
	path := out.(map[string]any)["pcap_path"].(string)
	if !strings.HasPrefix(path, "/tmp/ftw-pair-") || !strings.HasSuffix(path, ".pcap") {
		t.Fatalf("unexpected path: %s", path)
	}
}
