package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

// A margin-only save must keep the export choice the box holds when the write
// runs, even if another client changed it after this one read; and the
// reverse for an export-only save.
func TestPostPlannerPrefsChangesOnlyWhatIsSent(t *testing.T) {
	srv, ctrl, _ := plannerPrefsServer(t, control.ModePlannerArbitrage)
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/planner/prefs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}
	post := func(body string) map[string]any {
		t.Helper()
		rr := request(body)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %s: status=%d body=%s", body, rr.Code, rr.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	post(`{"safety_k": 0.3, "battery_export": "allowed"}`)
	post(`{"safety_k": 0.3, "battery_export": "not_allowed"}`) // another client turns sales off
	got := post(`{"safety_k": 0.6}`)
	if got["battery_export"] != "not_allowed" || got["safety_k"] != 0.6 {
		t.Fatalf("margin-only save = %v", got)
	}
	if _, export, k := srv.deps.PlannerPrefs.Get(); export != config.BatteryExportNotAllowed || k != 0.6 {
		t.Fatalf("stored export=%s k=%v", export, k)
	}
	if ctrl.Mode != control.ModePlannerPassiveArbitrage {
		t.Fatalf("mode = %s, want battery sales to stay off", ctrl.Mode)
	}
	// The export switch on another client keeps this client's margin.
	got = post(`{"battery_export": "allowed"}`)
	if got["battery_export"] != "allowed" || got["safety_k"] != 0.6 {
		t.Fatalf("export-only save = %v", got)
	}
	if ctrl.Mode != control.ModePlannerArbitrage {
		t.Fatalf("mode = %s, want battery sales back on", ctrl.Mode)
	}
	if rr := request(`{}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("an empty change got %d, want 400", rr.Code)
	}
}
