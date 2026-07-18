package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBootPhaseFailsHealthAndReadinessUntilStateIsReady(t *testing.T) {
	h := bootPhaseHandler()
	for _, path := range []string{"/api/health", "/api/status"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rr.Code)
			}
			if rr.Header().Get("Retry-After") == "" {
				t.Fatal("boot response must tell clients to retry")
			}
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if got := body["status"]; path == "/api/health" && got != "starting" {
				t.Fatalf("health status = %v, want starting", got)
			}
		})
	}
}
