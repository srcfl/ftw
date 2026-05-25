package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRestartMainServicePostsToAPI(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/version/restart" {
			t.Fatalf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		hit = true
		w.WriteHeader(202)
		w.Write([]byte(`{"status":"restarting"}`))
	}))
	defer srv.Close()

	tool := NewRestartMainServiceTool(srv.URL)
	out, err := tool.Handle(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("upstream not called")
	}
	if !out.(map[string]any)["ok"].(bool) {
		t.Fatal("expected ok:true")
	}
}

func TestRestartMainServiceSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()

	tool := NewRestartMainServiceTool(srv.URL)
	if _, err := tool.Handle(context.Background(), nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestTailServiceLogsGETsAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/logs" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		w.Write([]byte("Jan 01 00:00:00 host ftw[1]: started\n"))
	}))
	defer srv.Close()

	tool := NewTailServiceLogsTool(srv.URL)
	out, err := tool.Handle(context.Background(), map[string]any{"since": "10m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.(map[string]any)["log"].(string), "started") {
		t.Fatal("expected log content")
	}
}
