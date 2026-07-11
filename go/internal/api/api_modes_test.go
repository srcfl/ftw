package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/control"
)

// TestModesEndpoint verifies /api/modes serves the full canonical catalog
// (no Deps needed — it's static metadata) so the dashboard can build its mode
// buttons from the server instead of a hard-coded list.
func TestModesEndpoint(t *testing.T) {
	srv := New(&Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/modes", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/modes = %d, want 200", rec.Code)
	}

	var body struct {
		Modes []control.ModeInfo `json:"modes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Modes) != len(control.AllModes()) {
		t.Fatalf("got %d modes, want %d (all canonical modes)", len(body.Modes), len(control.AllModes()))
	}

	// Every advertised key must be a valid mode the /api/mode setter accepts,
	// and every canonical mode must be present — the endpoint is the UI's only
	// source of truth, so a gap or typo here is a broken or missing button.
	got := map[control.Mode]bool{}
	for _, m := range body.Modes {
		if !control.IsValidMode(m.Key) {
			t.Errorf("/api/modes advertised invalid mode %q", m.Key)
		}
		if m.Label == "" {
			t.Errorf("mode %q has empty label", m.Key)
		}
		got[m.Key] = true
	}
	for _, m := range control.AllModes() {
		if !got[m] {
			t.Errorf("/api/modes is missing canonical mode %q", m)
		}
	}
}
