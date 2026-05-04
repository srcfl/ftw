package configreload

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/control"
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

func watcherLoopCount() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return bytes.Count(buf[:n], []byte("configreload.(*Watcher).loop"))
}

func waitForWatcherLoop(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if watcherLoopCount() > baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watcher loop did not start")
}

// newTestWatcher creates a Watcher wired to track applier invocations.
// Returns the watcher plus an atomic counter and a channel that receives
// each (new, old) pair delivered to the applier.
func newTestWatcher(t *testing.T, cfgPath string, cfg *config.Config) (
	*Watcher, *atomic.Int32, chan [2]*config.Config,
) {
	t.Helper()
	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	ctrl := control.NewState(cfg.Site.GridTargetW, cfg.Site.GridToleranceW, cfg.SiteMeterDriver())

	var calls atomic.Int32
	applyCh := make(chan [2]*config.Config, 8)

	w, err := New(cfgPath, &cfgMu, cfg, &ctrlMu, ctrl, func(newCfg, oldCfg *config.Config) {
		calls.Add(1)
		applyCh <- [2]*config.Config{newCfg, oldCfg}
	})
	if err != nil {
		t.Fatal(err)
	}
	return w, &calls, applyCh
}

func TestWatcherFiresOnChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	w, _, applyCh := newTestWatcher(t, cfgPath, cfg)
	w.Start()
	defer w.Stop()

	// Modify the config: change grid_target_w from 0 to 100.
	updatedYAML := `
site:
  name: Test
  grid_target_w: 100
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
	// Small delay to let the watcher goroutine start and register.
	time.Sleep(100 * time.Millisecond)
	writeConfig(t, cfgPath, updatedYAML)

	select {
	case pair := <-applyCh:
		newCfg := pair[0]
		if newCfg.Site.GridTargetW != 100 {
			t.Errorf("expected grid_target_w=100, got %f", newCfg.Site.GridTargetW)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("applier not called within 3 s after config change")
	}
}

func TestWatcherIgnoresInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	w, calls, _ := newTestWatcher(t, cfgPath, cfg)
	w.Start()
	defer w.Stop()

	// Let the watcher start.
	time.Sleep(100 * time.Millisecond)

	// Write invalid YAML — config.Load will fail, reload() returns early,
	// and the applier should NOT be called.
	writeConfig(t, cfgPath, "{{{{not: valid: yaml: [")

	// Wait long enough for debounce (500 ms) + some margin.
	time.Sleep(1500 * time.Millisecond)

	if n := calls.Load(); n != 0 {
		t.Errorf("applier called %d times on invalid YAML; expected 0", n)
	}
}

func TestWatcherUpdatesSiteMeterDriverOnReload(t *testing.T) {
	// Operator moves `is_site_meter: true` from `ferroamp` to
	// `zap-p1` (typical when commissioning a real meter alongside
	// the sim). Without this change the dispatcher kept reading
	// from the old driver — grid_w pegged at 0 once the old
	// driver stopped emitting. The fix updates ctrl.SiteMeterDriver
	// inside the same ctrlMu block that gates dispatch reads of it.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	ctrl := control.NewState(cfg.Site.GridTargetW, cfg.Site.GridToleranceW, cfg.SiteMeterDriver())
	if ctrl.SiteMeterDriver != "ferroamp" {
		t.Fatalf("setup precondition: ctrl.SiteMeterDriver = %q, want ferroamp", ctrl.SiteMeterDriver)
	}

	applierCh := make(chan struct{}, 1)
	w, err := New(cfgPath, &cfgMu, cfg, &ctrlMu, ctrl, func(_, _ *config.Config) {
		applierCh <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Start()
	defer w.Stop()
	time.Sleep(100 * time.Millisecond) // let watcher subscribe

	// Two-driver YAML with the site-meter flag moved to zap-p1.
	updatedYAML := `
site:
  name: Test
  grid_target_w: 0
fuse:
  max_amps: 16
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    capabilities:
      mqtt:
        host: 192.168.1.153
  - name: zap-p1
    lua: drivers/esphome_dsmr.lua
    is_site_meter: true
    capabilities:
      http:
        allowed_hosts: ["192.168.1.147"]
    config:
      host: "192.168.1.147"
api:
  port: 8080
`
	writeConfig(t, cfgPath, updatedYAML)

	select {
	case <-applierCh:
	case <-time.After(3 * time.Second):
		t.Fatal("applier not called within 3 s after config change")
	}

	ctrlMu.Lock()
	got := ctrl.SiteMeterDriver
	ctrlMu.Unlock()
	if got != "zap-p1" {
		t.Errorf("after hot reload, ctrl.SiteMeterDriver = %q, want zap-p1", got)
	}
}

func TestWatcherStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	w, _, _ := newTestWatcher(t, cfgPath, cfg)
	w.Start()

	// First Stop should succeed normally.
	w.Stop()

	// Second Stop must not panic (guarded by sync.Once).
	w.Stop()
}

func TestWatcherStartIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, minimalYAML)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	w, calls, applyCh := newTestWatcher(t, cfgPath, cfg)
	baselineLoops := watcherLoopCount()
	w.Start()
	waitForWatcherLoop(t, baselineLoops)
	w.Start()
	defer w.Stop()

	time.Sleep(100 * time.Millisecond)
	if loops := watcherLoopCount() - baselineLoops; loops != 1 {
		t.Errorf("Start launched %d watcher loops; expected exactly 1", loops)
	}

	updatedYAML := `
site:
  name: Test
  grid_target_w: 100
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

	time.Sleep(100 * time.Millisecond)
	writeConfig(t, cfgPath, updatedYAML)

	select {
	case pair := <-applyCh:
		newCfg := pair[0]
		if newCfg.Site.GridTargetW != 100 {
			t.Errorf("expected grid_target_w=100, got %f", newCfg.Site.GridTargetW)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("applier not called within 3 s after config change")
	}

	time.Sleep(1500 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("applier called %d times after duplicate Start; expected exactly 1", n)
	}

	w.Stop()
	w.Stop()
}
