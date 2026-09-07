package configreload

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// minimalYAML is the smallest config that passes config.Load validation.
const minimalYAML = `
site:
  name: Test
  grid_target_w: 0
fuse:
  max_amps: 16
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    capabilities:
      mqtt:
        host: 192.168.1.153
api:
  port: 8080
`

// writeConfig writes YAML content to the config file.
func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyFirstSiteMeter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const noDriversYAML = `
site:
  name: Test
  grid_target_w: 0
fuse:
  max_amps: 16
drivers: []
api:
  port: 8080
`
	writeConfig(t, path, noDriversYAML)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, minimalYAML)
	newCfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	ctrl := control.NewState(0, 0, cfg.SiteMeterDriver())

	var gotNew, gotOld *config.Config
	Apply(&cfgMu, cfg, &ctrlMu, ctrl, newCfg, func(n, o *config.Config) {
		gotNew, gotOld = n, o
	})

	if ctrl.SiteMeterDriver != "ferroamp" {
		t.Fatalf("Ctrl.SiteMeterDriver = %q, want %q", ctrl.SiteMeterDriver, "ferroamp")
	}
	if cfg.SiteMeterDriver() != "ferroamp" {
		t.Fatalf("shared cfg not swapped: SiteMeterDriver() = %q", cfg.SiteMeterDriver())
	}
	if gotNew == nil || gotNew.SiteMeterDriver() != "ferroamp" {
		t.Fatal("applier did not receive the new config")
	}
	if gotOld == nil || gotOld.SiteMeterDriver() != "" {
		t.Fatal("applier did not receive the pre-apply snapshot as old")
	}
}
