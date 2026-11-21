package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// A charger whose driver only reads it says so on /api/loadpoints, with the
// driver's own reason when it gives one. A new owner added a Zaptec Go this
// way and FTW accepted it, never steered it and never said why.
func TestLoadpointsSayWhenTheChargerDriverOnlyReads(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("zaptec_cloud.lua", "DRIVER = {\n  id = \"zaptec_cloud\",\n  read_only = true,\n"+
		"  capabilities = { \"ev\" },\n  description = \"Zaptec Go via Zaptec Cloud.\",\n}\n")
	write("wall.lua", "DRIVER = {\n  id = \"wall\",\n  read_only = true,\n"+
		"  capabilities = { \"ev\" },\n  description = \"Tesla Wall Connector. Observation only; no cloud account.\",\n}\n")
	write("easee_cloud.lua", "DRIVER = {\n  id = \"easee_cloud\",\n  capabilities = { \"ev\" },\n}\n")

	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{
		{ID: "drive", DriverName: "zaptec", MinChargeW: 1380, MaxChargeW: 11000},
		{ID: "garage", DriverName: "wall", MinChargeW: 1380, MaxChargeW: 11000},
		{ID: "street", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 11000},
	})
	cfg := &config.Config{Drivers: []config.Driver{
		{Name: "zaptec", Lua: "drivers/zaptec_cloud.lua"},
		{Name: "wall", Lua: "drivers/wall.lua"},
		{Name: "easee", Lua: "drivers/easee_cloud.lua"},
	}}
	s := New(&Deps{Loadpoints: mgr, Cfg: cfg, CfgMu: &sync.RWMutex{}, DriverDir: dir})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/loadpoints", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Loadpoints []map[string]any `json:"loadpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, lp := range body.Loadpoints {
		byID[lp["id"].(string)] = lp
	}
	if byID["drive"]["driver_read_only"] != true {
		t.Errorf("zaptec loadpoint driver_read_only = %v, want true", byID["drive"]["driver_read_only"])
	}
	if _, ok := byID["drive"]["driver_read_only_reason"]; ok {
		t.Errorf("a product description is not a reason, got %v", byID["drive"]["driver_read_only_reason"])
	}
	if byID["garage"]["driver_read_only_reason"] != "Observation only; no cloud account." {
		t.Errorf("wall connector reason = %v", byID["garage"]["driver_read_only_reason"])
	}
	if _, ok := byID["street"]["driver_read_only"]; ok {
		t.Errorf("a controlling charger must not say read only: %v", byID["street"])
	}
}
