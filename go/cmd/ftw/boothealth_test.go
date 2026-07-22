package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBootPhaseDoesNotReportReadiness(t *testing.T) {
	handler := bootPhaseHandler()

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if health.Code != http.StatusOK || health.Body.String() != `{"status":"starting","phase":"initializing state"}` {
		t.Fatalf("boot liveness = %d %s", health.Code, health.Body.String())
	}

	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if status.Code != http.StatusServiceUnavailable {
		t.Fatalf("boot readiness = %d, want 503", status.Code)
	}
	if status.Header().Get("Retry-After") == "" {
		t.Fatal("boot readiness lacks Retry-After")
	}
}
