package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRestartMainServiceCallsSystemctl(t *testing.T) {
	var got []string
	r := &RestartMainServiceTool{run: func(ctx context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte("ok"), nil
	}}
	if _, err := r.Handle(context.Background(), nil); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if strings.Join(got, " ") != "systemctl restart forty-two-watts" {
		t.Fatalf("unexpected args: %v", got)
	}
}

func TestRestartMainServiceSurfacesError(t *testing.T) {
	r := &RestartMainServiceTool{run: func(context.Context, ...string) ([]byte, error) {
		return []byte("permission denied"), errors.New("exit 1")
	}}
	if _, err := r.Handle(context.Background(), nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestTailServiceLogsReadsJournalctl(t *testing.T) {
	tl := &TailServiceLogsTool{run: func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte("Jan 01 00:00:00 host ftw[1]: started\n"), nil
	}}
	out, err := tl.Handle(context.Background(), map[string]any{"since": "10m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.(map[string]any)["log"].(string), "started") {
		t.Fatalf("expected log content")
	}
}
