package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/loadmodel"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestHandleLoadModelProfileSwitch(t *testing.T) {
	lm := loadmodel.NewService(nil, telemetry.NewStore(), "site", 4000)
	srv := New(&Deps{LoadModel: lm})

	req := httptest.NewRequest(http.MethodPost, "/api/loadmodel/profile", strings.NewReader(`{"profile":"away"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("set profile status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if got := lm.Profile(); got != loadmodel.ProfileAway {
		t.Fatalf("profile = %q, want away", got)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/loadmodel", nil)
	getRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get loadmodel status = %d, body: %s", getRR.Code, getRR.Body.String())
	}
	var resp struct {
		Profile  string                     `json:"profile"`
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(getRR.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Profile != "away" {
		t.Fatalf("response profile = %q, want away", resp.Profile)
	}
	if _, ok := resp.Profiles["home"]; !ok {
		t.Fatalf("home profile missing from response: %s", getRR.Body.String())
	}
	if _, ok := resp.Profiles["away"]; !ok {
		t.Fatalf("away profile missing from response: %s", getRR.Body.String())
	}
}

func TestHandleLoadModelProfileRejectsUnknown(t *testing.T) {
	lm := loadmodel.NewService(nil, telemetry.NewStore(), "site", 4000)
	srv := New(&Deps{LoadModel: lm})

	req := httptest.NewRequest(http.MethodPost, "/api/loadmodel/profile", strings.NewReader(`{"profile":"vacation"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
}
