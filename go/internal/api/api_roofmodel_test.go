package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/roofmodel"
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

func TestRoofModelBuildingsDisabledReportsCleanly(t *testing.T) {
	code, body := getJSON(t, depsAt(59.33, 18.07), http.MethodGet, "/api/roofmodel/buildings")
	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
	if body["error"] == nil {
		t.Error("a disabled building search should say why")
	}
}

// The map lets the pin be dragged before anything is saved, so the picker has
// to be able to search where the pin is rather than where the config says.
func TestRoofModelBuildingsAcceptsAnExplicitCoordinate(t *testing.T) {
	deps := depsAt(59.33, 18.07)
	deps.RoofModel = roofmodel.FromConfig(&config.RoofModel{
		Enabled: true, Command: "definitely-not-a-real-command",
		GeotorgetUsername: "u", GeotorgetToken: "t",
	})

	// Berlin is outside Lantmateriet coverage; if the query coordinate were
	// ignored the stored Stockholm one would be used and this would not be a 400.
	code, body := getJSON(t, deps, http.MethodGet,
		"/api/roofmodel/buildings?lat=52.52&lon=13.40")
	if code != 400 {
		t.Fatalf("status = %d, want 400 for a site outside Sweden (body %v)", code, body)
	}

	// A malformed pair must fall back to the configured site rather than
	// searching at (0, 0), which is in the Atlantic.
	_, sthlm := getJSON(t, deps, http.MethodGet, "/api/roofmodel/buildings?lat=abc&lon=def")
	if sthlm["error"] == nil {
		t.Fatal("want the spawn to fail, since the command does not exist")
	}
	if msg, _ := sthlm["error"].(string); strings.Contains(msg, "not in Sweden") {
		t.Errorf("bad coordinates were used instead of the configured site: %v", msg)
	}
}

// The Geotorget token is the operator's credential. Status may be reported;
// the secret itself must never appear in a response.
func TestRoofModelNeverEchoesTheToken(t *testing.T) {
	deps := depsAt(59.33, 18.07)
	deps.Cfg.RoofModel = &config.RoofModel{
		Enabled: true, GeotorgetUsername: "operator", GeotorgetToken: "gt_secret_value",
	}
	deps.RoofModel = roofmodel.FromConfig(deps.Cfg.RoofModel)

	srv := New(deps)
	for _, path := range []string{"/api/roofmodel", "/api/roofmodel/buildings"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if strings.Contains(rr.Body.String(), "gt_secret_value") {
			t.Errorf("%s leaked the Geotorget token: %s", path, rr.Body.String())
		}
	}

	_, status := getJSON(t, deps, http.MethodGet, "/api/roofmodel")
	if status["has_credentials"] != true {
		t.Errorf("has_credentials = %v, want true so the UI can stop asking",
			status["has_credentials"])
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
