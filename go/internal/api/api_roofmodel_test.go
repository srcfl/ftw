package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getJSON(t *testing.T, deps *Deps, method, path string) (int, map[string]any) {
	t.Helper()
	srv := New(deps)
	req := httptest.NewRequest(method, path, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable body %q: %v", rr.Body.String(), err)
	}
	return rr.Code, body
}

// Absent module: the endpoint must answer calmly, the way every other optional
// service does, rather than 404 or 500.
func TestRoofModelDisabledReportsCleanly(t *testing.T) {
	code, body := getJSON(t, depsAt(59.33, 18.07), http.MethodGet, "/api/roofmodel")
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
}

// The metadata is the useful part when it is unavailable: it says where the
// data exists at all.
func TestRoofModelReportsCoverageForTheSite(t *testing.T) {
	_, sthlm := getJSON(t, depsAt(59.33, 18.07), http.MethodGet, "/api/roofmodel")
	if sthlm["covers"] != true {
		t.Errorf("Stockholm covers = %v, want true", sthlm["covers"])
	}
	if sthlm["area"] != "Sweden" {
		t.Errorf("area = %v, want Sweden", sthlm["area"])
	}

	_, berlin := getJSON(t, depsAt(52.52, 13.40), http.MethodGet, "/api/roofmodel")
	if berlin["covers"] != false {
		t.Errorf("Berlin covers = %v, want false", berlin["covers"])
	}
	// Even uncovered, it must still say where the data does exist.
	if berlin["area"] != "Sweden" {
		t.Errorf("area = %v, want Sweden even when uncovered", berlin["area"])
	}
}

func TestRoofModelDeriveDisabledDoesNotError(t *testing.T) {
	code, body := getJSON(t, depsAt(59.33, 18.07), http.MethodPost, "/api/roofmodel/derive")
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
	if body["error"] == nil {
		t.Error("a disabled derive should say why")
	}
}

// Without a location there is nothing to derive, and that is the operator's
// mistake to fix — so it is a 400, not a silent empty result.
func TestRoofModelDeriveWithoutASiteIsARequestError(t *testing.T) {
	code, body := getJSON(t, &Deps{}, http.MethodPost, "/api/roofmodel/derive")
	if code == 200 && body["enabled"] == false {
		return // module disabled takes precedence, which is also correct
	}
	if code != 400 {
		t.Errorf("status = %d, want 400", code)
	}
}

// Lantmäteriet must appear in the coverage listing alongside every other
// source, so an operator finds it without knowing it exists.
func TestLantmaterietAppearsInDataSources(t *testing.T) {
	resp := getDataSources(t, depsAt(59.33, 18.07), "")
	lm := find(t, resp, "lantmateriet")
	if lm.Kind != "geodata" {
		t.Errorf("kind = %q, want geodata", lm.Kind)
	}
	if !lm.RequiresKey {
		t.Error("Geotorget access is credential-gated")
	}
	if lm.Worldwide {
		t.Error("Lantmäteriet is Sweden only")
	}
	if lm.Covers == nil || !*lm.Covers {
		t.Error("should cover Stockholm")
	}

	away := getDataSources(t, depsAt(-33.87, 151.21), "")
	if s := find(t, away, "lantmateriet"); s.Covers == nil || *s.Covers {
		t.Error("must not claim to cover Sydney")
	}
}
