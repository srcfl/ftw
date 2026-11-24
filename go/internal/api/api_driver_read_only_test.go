package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The Wall Connector's description at srcfl/device-drivers c4c39af.
const wallConnectorDescription = "Reads a Tesla Wall Connector Gen 3 on your network. FTW can read it but not control it: Tesla offers no way for another system to set its current. Control goes through the car (tesla_vehicle) or a charger FTW can control, such as one on OCPP."

const wallConnectorReason = "Tesla offers no way for another system to set its current. Control goes through the car (tesla_vehicle) or a charger FTW can control, such as one on OCPP."

func writeDriverFile(t *testing.T, dir, name string, readOnly bool, description string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf("DRIVER = {\n  id = %q,\n  capabilities = { \"ev\" },\n  read_only = %t,\n  description = %q,\n}\n"+
		"function driver_init(config) end\nfunction driver_poll() return 1000 end\n"+
		"function driver_command(action, w, cmd) return true end\nfunction driver_default_mode() end\n",
		name, readOnly, description)
	path := filepath.Join(dir, name+".lua")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// chargerLoadpoints configures one loadpoint per driver and returns
// /api/loadpoints by loadpoint id.
func chargerLoadpoints(t *testing.T, deps *Deps, driverNames ...string) map[string]map[string]any {
	t.Helper()
	mgr := loadpoint.NewManager()
	var cfgs []loadpoint.Config
	for _, name := range driverNames {
		cfgs = append(cfgs, loadpoint.Config{ID: "lp-" + name, DriverName: name, MinChargeW: 1380, MaxChargeW: 11000})
	}
	mgr.Load(cfgs)
	deps.Loadpoints = mgr
	if deps.CfgMu == nil {
		deps.CfgMu = &sync.RWMutex{}
	}
	s := New(deps)
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
	return byID
}

// A charger whose driver only reads it says so on /api/loadpoints, with the
// driver's own reason when it gives one. A new owner added a Zaptec Go this
// way and FTW accepted it, never steered it and never said why.
func TestLoadpointsSayWhenTheChargerDriverOnlyReads(t *testing.T) {
	bundled := t.TempDir()
	writeDriverFile(t, bundled, "zaptec_cloud", true, "Zaptec Go via Zaptec Cloud.")
	writeDriverFile(t, bundled, "wall", true, wallConnectorDescription)
	writeDriverFile(t, bundled, "easee_cloud", false, "Easee via Cloud.")
	cfg := &config.Config{Drivers: []config.Driver{
		{Name: "zaptec", Lua: "drivers/zaptec_cloud.lua"},
		{Name: "wall", Lua: "drivers/wall.lua"},
		{Name: "easee", Lua: "drivers/easee_cloud.lua"},
		{Name: "watched", Lua: "drivers/easee_cloud.lua", ObserveOnly: true},
	}}
	byID := chargerLoadpoints(t, &Deps{Cfg: cfg, DriverDir: bundled}, "zaptec", "wall", "easee", "watched")

	if byID["lp-zaptec"]["driver_read_only"] != true {
		t.Errorf("zaptec driver_read_only = %v, want true", byID["lp-zaptec"]["driver_read_only"])
	}
	if _, ok := byID["lp-zaptec"]["driver_read_only_reason"]; ok {
		t.Errorf("a product description is not a reason, got %v", byID["lp-zaptec"]["driver_read_only_reason"])
	}
	if got := byID["lp-wall"]["driver_read_only_reason"]; got != wallConnectorReason {
		t.Errorf("wall connector reason = %v", got)
	}
	if _, ok := byID["lp-easee"]["driver_read_only"]; ok {
		t.Errorf("a controlling charger must not say read only: %v", byID["lp-easee"])
	}
	// observe_only keeps FTW from steering the charger as surely as a
	// read-only driver, so it reads the same, with the owner's own setting
	// as the reason.
	if byID["lp-watched"]["driver_read_only"] != true || byID["lp-watched"]["driver_read_only_reason"] != observeOnlyReason {
		t.Errorf("observe-only charger = %v", byID["lp-watched"])
	}
}

// A running config holds resolved absolute paths, and a driver in the user
// folder shadows the release's copy of the same name. The answer has to
// come from the file that runs.
func TestLoadpointReadOnlyFollowsTheFileThatRuns(t *testing.T) {
	bundled := t.TempDir()
	user := t.TempDir()
	elsewhere := t.TempDir()
	writeDriverFile(t, bundled, "zaptec_cloud", true, "Zaptec Go via Zaptec Cloud. Read-only: this copy has no control path written yet.")
	writeDriverFile(t, user, "zaptec_cloud", false, "Zaptec Go via Zaptec Cloud, with control.")
	writeDriverFile(t, bundled, "wall", false, "A copy that must not be read.")
	absolute := writeDriverFile(t, elsewhere, "wall", true, wallConnectorDescription)
	cfg := &config.Config{Drivers: []config.Driver{
		{Name: "zaptec", Lua: "drivers/zaptec_cloud.lua"},
		{Name: "wall", Lua: absolute},
	}}
	byID := chargerLoadpoints(t, &Deps{Cfg: cfg, DriverDir: bundled, UserDriverDir: user}, "zaptec", "wall")
	if _, ok := byID["lp-zaptec"]["driver_read_only"]; ok {
		t.Errorf("the user folder's controlling copy runs, got %v", byID["lp-zaptec"])
	}
	if byID["lp-wall"]["driver_read_only"] != true || byID["lp-wall"]["driver_read_only_reason"] != wallConnectorReason {
		t.Errorf("absolute path = %v", byID["lp-wall"])
	}
}

// The registry's loaded driver decides; the file only gives the reason. A
// file edited after the driver loaded does not change what the box does
// until the driver reloads, so it must not change what the box says.
func TestLoadpointReadOnlyAsksTheLoadedDriver(t *testing.T) {
	dir := t.TempDir()
	reg := drivers.NewRegistry(telemetry.NewStore())
	t.Cleanup(reg.ShutdownAll)
	controls := writeDriverFile(t, dir, "controls", false, "Controls a charger.")
	reads := writeDriverFile(t, dir, "reads", true, wallConnectorDescription)
	for name, path := range map[string]string{"controls": controls, "reads": reads} {
		if err := reg.Add(context.Background(), config.Driver{Name: name, Lua: path}); err != nil {
			t.Fatal(err)
		}
	}
	writeDriverFile(t, dir, "controls", true, "Edited after it loaded. Read-only: not yet reloaded by the box.")
	writeDriverFile(t, dir, "reads", false, "Edited after it loaded, with control.")
	cfg := &config.Config{Drivers: []config.Driver{{Name: "controls", Lua: controls}, {Name: "reads", Lua: reads}}}
	byID := chargerLoadpoints(t, &Deps{Cfg: cfg, Registry: reg}, "controls", "reads")
	if _, ok := byID["lp-controls"]["driver_read_only"]; ok {
		t.Errorf("the loaded driver controls, got %v", byID["lp-controls"])
	}
	if byID["lp-reads"]["driver_read_only"] != true {
		t.Errorf("the loaded driver reads only, got %v", byID["lp-reads"])
	}
}

// /api/loadpoints is read on every status poll. A driver file is parsed once
// and again only after it changes.
func TestLoadpointReadOnlyParsesADriverFileOnce(t *testing.T) {
	dir := t.TempDir()
	path := writeDriverFile(t, dir, "wall", true, wallConnectorDescription)
	parses := 0
	orig := parseCatalogFile
	parseCatalogFile = func(p string) (drivers.CatalogEntry, error) {
		if p == path {
			parses++
		}
		return orig(p)
	}
	t.Cleanup(func() { parseCatalogFile = orig })
	cfg := &config.Config{Drivers: []config.Driver{{Name: "wall", Lua: path}}}
	s := New(&Deps{Cfg: cfg, CfgMu: &sync.RWMutex{}})
	for i := 0; i < 3; i++ {
		if got := s.readOnlyDrivers()["wall"].Reason; got != wallConnectorReason {
			t.Fatalf("reason = %q", got)
		}
	}
	if parses != 1 {
		t.Fatalf("parsed %d times for three reads, want 1", parses)
	}
	writeDriverFile(t, dir, "wall", true, "Reads the charger. Read-only: the vendor offers no way to set its current from here.")
	if got := s.readOnlyDrivers()["wall"].Reason; got != "The vendor offers no way to set its current from here." {
		t.Fatalf("after an edit, reason = %q", got)
	}
	if parses != 2 {
		t.Fatalf("parsed %d times, want 2 after one edit", parses)
	}
}
