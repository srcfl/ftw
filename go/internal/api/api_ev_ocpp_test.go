package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// An OCPP charge point has no Lua driver — it dialled us rather than being
// dialled — so it is not in the driver registry. Sending the dashboard's
// Pause / Resume / Force start straight to the registry finds no such driver
// and fails, which reads as the charger being broken rather than unrouted.
//
// Found by running the branch against Sourceful's device simulator: automatic
// dispatch steered the charger and every manual control returned
// `driver "garage" not found`.
func TestEVCommandReachesAChargerWithNoDriver(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("garage", telemetry.DerEV, 0, nil, nil)

	var mu sync.Mutex
	var gotName string
	var gotAction string

	srv := New(&Deps{
		Tel: tel,
		// Registry deliberately nil: this is a site whose only charger is an
		// OCPP one, so there is no Lua driver registry to fall back to.
		EVSend: func(_ context.Context, name string, payload []byte) error {
			var cmd struct {
				Action string `json:"action"`
			}
			_ = json.Unmarshal(payload, &cmd)
			mu.Lock()
			gotName, gotAction = name, cmd.Action
			mu.Unlock()
			return nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/ev/command",
		strings.NewReader(`{"action":"ev_pause","driver":"garage"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotName != "garage" {
		t.Errorf("command went to %q, want garage", gotName)
	}
	if gotAction != "ev_pause" {
		t.Errorf("action: got %q, want ev_pause", gotAction)
	}
}

// With neither route available the endpoint still refuses rather than
// pretending the command landed.
func TestEVCommandWithNoRouteIsRefused(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("garage", telemetry.DerEV, 0, nil, nil)
	srv := New(&Deps{Tel: tel})

	req := httptest.NewRequest(http.MethodPost, "/api/ev/command",
		strings.NewReader(`{"action":"ev_pause","driver":"garage"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503 (body: %s)", rr.Code, rr.Body.String())
	}
}
