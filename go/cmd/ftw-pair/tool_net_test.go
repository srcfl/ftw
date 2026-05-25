package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "ProbeTest/1.0")
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	}))
	defer srv.Close()

	tool := NewHTTPProbeTool()
	out, err := tool.Handle(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["status"].(int) != 200 {
		t.Fatalf("status: %v", m["status"])
	}
	if !strings.Contains(m["body"].(string), "OK") {
		t.Fatalf("body: %v", m["body"])
	}
}

func TestNetworkScanReadsArpFile(t *testing.T) {
	tool := &NetworkScanTool{
		arpReader: func() (string, error) {
			return `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.10     0x1         0x2         aa:bb:cc:dd:ee:ff     *        eth0
192.168.1.20     0x1         0x2         11:22:33:44:55:66     *        eth0`, nil
		},
	}
	out, err := tool.Handle(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := out.(map[string]any)["entries"].([]map[string]string)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0]["ip"] != "192.168.1.10" {
		t.Fatalf("first ip: %v", entries[0])
	}
}
