package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

const deviceSupportYAML = `
site:
  name: Test
fuse:
  max_amps: 16
drivers:
  - name: inverter
    lua: drivers/sungrow.lua
    is_site_meter: true
    capabilities:
      modbus:
        host: 192.168.1.10
    control:
      enabled: true
      package_id: com.sourceful.driver.sungrow
      version: 1.2.3
      artifact_sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
device_repository:
  enabled: true
  repositories:
    - id: device-support
      format: sourceful.driver-index/v1
      manifest_url: https://packages.example/index.json
      enabled: true
      trusted_keys:
        sourceful-1: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
    - id: custom
      manifest_url: https://drivers.example/manifest.json
      enabled: true
      trusted_keys:
        test: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
`

func TestLoadDropsDeviceSupportRepositoryAndControlOptIn(t *testing.T) {
	cfg, err := Parse([]byte(deviceSupportYAML), "/tmp")
	if err != nil {
		t.Fatalf("a config with Device Support settings must still load: %v", err)
	}
	repos := cfg.DeviceRepository.Repositories
	if len(repos) != 1 || repos[0].ID != "custom" {
		t.Fatalf("repositories = %+v; want only the FTW manifest source", repos)
	}
	if len(cfg.Retired) != 2 ||
		!strings.Contains(cfg.Retired[0], `"device-support"`) ||
		!strings.Contains(cfg.Retired[1], `"inverter"`) || !strings.Contains(cfg.Retired[1], "no effect") {
		t.Fatalf("retired notices = %q; want one naming the repository and one naming the driver", cfg.Retired)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), retiredDriverRepositoryFormat) || strings.Contains(string(raw), "com.sourceful.driver") {
		t.Fatalf("retired settings survive loading: %s", raw)
	}
}

func TestLoadKeepsTheDefaultChannelWhenOnlyADeviceSupportSourceWasListed(t *testing.T) {
	yaml := strings.Replace(deviceSupportYAML, `    - id: custom
      manifest_url: https://drivers.example/manifest.json
      enabled: true
      trusted_keys:
        test: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
`, "", 1)
	cfg, err := Parse([]byte(yaml), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	repos := cfg.DeviceRepository.Repositories
	if len(repos) != 1 || repos[0].ID != DefaultDriverRepositoryID || repos[0].ManifestURL != DefaultDriverRepositoryManifestURL {
		t.Fatalf("repositories = %+v; want FTW's default signed channel", repos)
	}
}

func TestStoredDeviceSupportSettingsLoadAndAreDeleted(t *testing.T) {
	dir := t.TempDir()
	path, database := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.db")
	if err := os.WriteFile(path, []byte(minimalYAML), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := InitializeStorage(path, database, cfg, st); err != nil {
		t.Fatal(err)
	}
	// Settings saved by a Core that still read Device Support packages.
	current, _, err := st.Configuration()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(current.Document, &doc); err != nil {
		t.Fatal(err)
	}
	stored := doc["config"].(map[string]any)
	repository := stored["device_repository"].(map[string]any)
	repository["repositories"] = append(repository["repositories"].([]any), map[string]any{
		"id": "device-support", "format": "sourceful.driver-index/v1", "enabled": true,
		"manifest_url": "https://packages.example/index.json",
		"trusted_keys": map[string]any{"sourceful-1": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
	})
	stored["drivers"].([]any)[0].(map[string]any)["control"] = map[string]any{
		"enabled": true, "package_id": "com.sourceful.driver.ferroamp", "version": "1.0.0",
		"artifact_sha256": strings.Repeat("a", 64),
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveConfiguration(raw, current.Revision, nil); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("stored Device Support settings stopped startup: %v", err)
	}
	for _, repo := range loaded.DeviceRepository.Repositories {
		if repo.ID == "device-support" {
			t.Fatal("the Device Support repository still steers the site")
		}
	}
	if len(loaded.Retired) != 2 {
		t.Fatalf("retired notices = %q; want the repository and the control opt-in", loaded.Retired)
	}
	if _, err := InitializeStorage(path, database, loaded, st); err != nil {
		t.Fatalf("startup storage check after dropping retired settings: %v", err)
	}

	removed, err := DropRetiredSettings(st, path, loaded)
	if err != nil || len(removed) != 2 {
		t.Fatalf("DropRetiredSettings = %v, %v; want the repository and the control opt-in", removed, err)
	}
	after, _, err := st.Configuration()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after.Document), retiredDriverRepositoryFormat) ||
		strings.Contains(string(after.Document), `"control"`) {
		t.Fatalf("stored settings still carry Device Support settings: %s", after.Document)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Retired) != 0 {
		t.Fatalf("retired notices after cleanup = %q", reloaded.Retired)
	}
	if removed, err := DropRetiredSettings(st, path, reloaded); err != nil || len(removed) != 0 {
		t.Fatalf("second DropRetiredSettings = %v, %v; want nothing", removed, err)
	}
}
