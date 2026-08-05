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
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const controlDriverLua = `DRIVER = {
  id      = "probe",
  name    = "Probe",
  version = "1.0.0",
  controls = {
    {
      id       = "set_heat_curve_offset",
      label    = "Heat curve offset",
      evidence = "readback",
      input    = { type = "number", min = -3, max = 3, step = 1, unit = "°C" },
    },
  },
}
`

const quietDriverLua = `DRIVER = {
  id      = "quiet",
  name    = "Quiet",
  version = "1.0.0",
}
`

// serverWithDrivers wires the smallest Deps that /api/drivers/{name} needs:
// a driver directory to parse and a config naming the file, since the lookup
// goes driver name → configured lua path → catalog entry.
func serverWithDrivers(t *testing.T, files map[string]string, cfg *config.Config) *Server {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range cfg.Drivers {
		cfg.Drivers[i].Lua = filepath.Join(dir, cfg.Drivers[i].Lua)
	}
	return New(&Deps{
		Tel:        telemetry.NewStore(),
		Cfg:        cfg,
		CfgMu:      &sync.RWMutex{},
		DriverDir:  dir,
		ConfigPath: filepath.Join(dir, "config.yaml"),
	})
}

func driverDetail(t *testing.T, srv *Server, name string) driverDetailResp {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drivers/"+name, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/drivers/%s = %d, body %s", name, rec.Code, rec.Body.String())
	}
	var got driverDetailResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestDriverDetailSurfacesDeclaredControls(t *testing.T) {
	srv := serverWithDrivers(t,
		map[string]string{"probe.lua": controlDriverLua},
		&config.Config{Drivers: []config.Driver{{Name: "heat", Lua: "probe.lua"}}})

	got := driverDetail(t, srv, "heat")
	if len(got.Controls) != 1 {
		t.Fatalf("controls = %+v, want 1", got.Controls)
	}
	c := got.Controls[0]
	if c.ID != "set_heat_curve_offset" || c.Label != "Heat curve offset" ||
		c.Evidence != "readback" {
		t.Errorf("control = %+v", c)
	}
	if c.Input.Type != "number" || c.Input.Unit != "°C" {
		t.Errorf("input = %+v", c.Input)
	}
	if c.Input.Min == nil || *c.Input.Min != -3 || c.Input.Max == nil || *c.Input.Max != 3 {
		t.Errorf("bounds = %v..%v", c.Input.Min, c.Input.Max)
	}
}

// A reporting-only driver must not grow a controls key. The UI decides
// whether to render anything by its presence, so an empty list and no list
// have to stay distinguishable in the JSON.
func TestDriverDetailOmitsControlsWhenNoneDeclared(t *testing.T) {
	srv := serverWithDrivers(t,
		map[string]string{"quiet.lua": quietDriverLua},
		&config.Config{Drivers: []config.Driver{{Name: "quiet", Lua: "quiet.lua"}}})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drivers/quiet", nil))
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["controls"]; present {
		t.Errorf("controls key present for a reporting-only driver: %s", rec.Body.String())
	}
}

// The declaration belongs to the driver file, not to the name in YAML. Two
// drivers on the same file both get it; a name that matches no configured
// driver gets nothing.
func TestDriverDetailResolvesControlsByFileNotName(t *testing.T) {
	srv := serverWithDrivers(t,
		map[string]string{"probe.lua": controlDriverLua, "quiet.lua": quietDriverLua},
		&config.Config{Drivers: []config.Driver{
			{Name: "upstairs", Lua: "probe.lua"},
			{Name: "downstairs", Lua: "probe.lua"},
			{Name: "meter", Lua: "quiet.lua"},
		}})

	for _, name := range []string{"upstairs", "downstairs"} {
		if got := driverDetail(t, srv, name); len(got.Controls) != 1 {
			t.Errorf("%s: controls = %+v, want 1", name, got.Controls)
		}
	}
	if got := driverDetail(t, srv, "meter"); len(got.Controls) != 0 {
		t.Errorf("meter: controls = %+v, want none", got.Controls)
	}
	if got := driverDetail(t, srv, "not-configured"); len(got.Controls) != 0 {
		t.Errorf("unknown driver: controls = %+v, want none", got.Controls)
	}
}

func TestDriverDetailUsesConfiguredFileWhenFilenameIsShadowed(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "user")
	bundledDir := filepath.Join(root, "bundled")
	for _, dir := range []string{userDir, bundledDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	localLua := `DRIVER = {
  id = "local",
  name = "Local shadow",
  controls = {
    { id = "local_control", input = { type = "number", min = 0, max = 1 } },
  },
}
`
	if err := os.WriteFile(filepath.Join(userDir, "foo.lua"), []byte(localLua), 0o600); err != nil {
		t.Fatal(err)
	}
	bundledLua := `DRIVER = {
  id = "bundled",
  name = "Bundled selected",
  controls = {
    { id = "bundled_control", input = { type = "number", min = 0, max = 1 } },
  },
}
`
	configured := filepath.Join(bundledDir, "foo.lua")
	if err := os.WriteFile(configured, []byte(bundledLua), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := New(&Deps{
		Tel:           telemetry.NewStore(),
		Cfg:           &config.Config{Drivers: []config.Driver{{Name: "heat", Lua: configured}}},
		CfgMu:         &sync.RWMutex{},
		UserDriverDir: userDir,
		DriverDir:     bundledDir,
		ConfigPath:    filepath.Join(root, "config.yaml"),
	})
	got := driverDetail(t, srv, "heat")
	if len(got.Controls) != 1 || got.Controls[0].ID != "bundled_control" {
		t.Fatalf("controls = %+v, want the configured bundled file", got.Controls)
	}
}
