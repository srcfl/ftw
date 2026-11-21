package drivers

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCatalogUsesPortableDriverPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custom-driver-dir")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "custom.lua"), []byte(`DRIVER = {
  id = "custom"
  name = "Custom"
}
`), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := LoadCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("LoadCatalog returned %d entries, want 1: %+v", len(entries), entries)
	}
	if entries[0].Path != "drivers/custom.lua" {
		t.Fatalf("catalog path = %q, want portable drivers/custom.lua", entries[0].Path)
	}
}

func TestLoadCatalogMultiUnionAndFirstWins(t *testing.T) {
	userDir := filepath.Join(t.TempDir(), "user-drivers")
	bundledDir := filepath.Join(t.TempDir(), "bundled-drivers")
	if err := os.Mkdir(userDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bundledDir, 0755); err != nil {
		t.Fatal(err)
	}

	// shared.lua exists in both dirs — user version should win.
	userShared := "DRIVER = {\n  id = \"shared_user\",\n  name = \"Shared User\"\n}\n"
	bundledShared := "DRIVER = {\n  id = \"shared_bundled\",\n  name = \"Shared Bundled\"\n}\n"
	if err := os.WriteFile(filepath.Join(userDir, "shared.lua"), []byte(userShared), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundledDir, "shared.lua"), []byte(bundledShared), 0644); err != nil {
		t.Fatal(err)
	}

	// bundled.lua only in bundled dir.
	if err := os.WriteFile(filepath.Join(bundledDir, "bundled.lua"), []byte("DRIVER = {\n  id = \"bundled_only\",\n  name = \"Bundled Only\"\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := LoadCatalogMulti(userDir, bundledDir)
	if err != nil {
		t.Fatalf("LoadCatalogMulti: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
	}

	byFilename := make(map[string]CatalogEntry)
	for _, e := range entries {
		byFilename[e.Filename] = e
	}

	if e, ok := byFilename["shared.lua"]; !ok {
		t.Fatal("shared.lua missing from catalog")
	} else if e.ID != "shared_user" {
		t.Errorf("shared.lua: want id=shared_user (user wins), got %q", e.ID)
	}

	if _, ok := byFilename["bundled.lua"]; !ok {
		t.Fatal("bundled.lua missing from catalog (union should include it)")
	}
}

func TestLoadCatalogMultiMissingDirSkipped(t *testing.T) {
	bundledDir := filepath.Join(t.TempDir(), "bundled")
	if err := os.Mkdir(bundledDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundledDir, "drv.lua"), []byte("DRIVER = {\n  id = \"x\",\n  name = \"X\"\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// nonexistent is fine — should be silently skipped.
	entries, err := LoadCatalogMulti("/nonexistent/path/that/does/not/exist", bundledDir)
	if err != nil {
		t.Fatalf("LoadCatalogMulti: unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry from bundledDir, got %d", len(entries))
	}
}

// A write path only reaches the Settings UI if the driver says it has one.
// The alternative — FTW recognising "the NIBE driver can write" by filename
// — puts vendor knowledge in the host and silently arms a write switch for
// any file that happens to be named the same.
func TestLoadCatalogReadsWriteCapabilities(t *testing.T) {
	dir := t.TempDir()
	writer := "DRIVER = {\n  id = \"nibe-local\",\n  name = \"NIBE\",\n" +
		"  write_capabilities = { \"solar_pv\" },\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "nibe_local.lua"), []byte(writer), 0644); err != nil {
		t.Fatal(err)
	}
	// A driver that mentions writing in prose still declares nothing.
	reader := "-- writes nothing; write_capabilities stays unset\n" +
		"DRIVER = {\n  id = \"reader\",\n  name = \"Reader\",\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "reader.lua"), []byte(reader), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := LoadCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	byID := make(map[string]CatalogEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}

	got := byID["nibe-local"].WriteCapabilities
	if len(got) != 1 || got[0] != "solar_pv" {
		t.Errorf("nibe-local WriteCapabilities = %v, want [solar_pv]", got)
	}
	if len(byID["reader"].WriteCapabilities) != 0 {
		t.Errorf("reader declared no write path but got %v", byID["reader"].WriteCapabilities)
	}
}

func TestLoadCatalogReadsAuthPostPath(t *testing.T) {
	dir := t.TempDir()
	oauth := "DRIVER = {\n  id = \"myuplink\",\n  name = \"MyUplink\",\n" +
		"  read_only = true,\n  auth_post_path = \"/oauth/token\",\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "myuplink.lua"), []byte(oauth), 0644); err != nil {
		t.Fatal(err)
	}
	plain := "DRIVER = {\n  id = \"meter\",\n  name = \"Meter\",\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "meter.lua"), []byte(plain), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := LoadCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	byID := make(map[string]CatalogEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	if byID["myuplink"].AuthPostPath != "/oauth/token" {
		t.Errorf("myuplink AuthPostPath = %q, want /oauth/token", byID["myuplink"].AuthPostPath)
	}
	if byID["meter"].AuthPostPath != "" {
		t.Errorf("meter AuthPostPath = %q, want empty", byID["meter"].AuthPostPath)
	}
}

func TestLoadCatalogReadsAuthPostPaths(t *testing.T) {
	dir := t.TempDir()
	src := "DRIVER = {\n  id = \"vag\",\n  name = \"VAG\",\n  read_only = true,\n" +
		"  auth_post_paths = { \"/a/login/identifier\", \"/a/login/authenticate\" },\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "vag.lua"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	entries, err := LoadCatalog(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("LoadCatalog: %v %v", entries, err)
	}
	got := entries[0].AuthPostPaths
	if len(got) != 2 || got[0] != "/a/login/identifier" || got[1] != "/a/login/authenticate" {
		t.Fatalf("AuthPostPaths = %q", got)
	}
}

func TestCatalogMyUplinkDeclaresAuthPostPath(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	for _, e := range entries {
		if e.ID == "myuplink" {
			if e.AuthPostPath != "/oauth/token" {
				t.Fatalf("myuplink AuthPostPath = %q, want /oauth/token", e.AuthPostPath)
			}
			return
		}
	}
	t.Fatal("myuplink missing from catalog")
}

// A read-only driver's description is its reason only when it says why, and
// without a lead-in that repeats the note the owner already reads. The
// descriptions are the drivers' own at srcfl/device-drivers c4c39af.
func TestLoadCatalogReadOnlyReason(t *testing.T) {
	dir := t.TempDir()
	driver := func(id, caps string, readOnly bool, description string) string {
		return fmt.Sprintf("DRIVER = {\n  id = %q,\n  capabilities = { %s },\n  read_only = %t,\n  description = %q,\n}\n",
			id, caps, readOnly, description)
	}
	files := map[string]string{
		"tesla_wall_connector.lua": driver("tesla_wall_connector", `"ev"`, true,
			"Reads a Tesla Wall Connector Gen 3 on your network. FTW can read it but not control it: Tesla offers no way for another system to set its current. Control goes through the car (tesla_vehicle) or a charger FTW can control, such as one on OCPP."),
		"tesla_cloud.lua": driver("tesla_cloud", `"vehicle"`, true,
			"Reads a Tesla's charge level, limit, charging state and cable from Tesla's Fleet API. Read-only: Tesla accepts a command only when it is signed with a key paired to the car, and this driver cannot sign. Use tesla_vehicle (BLE) to start the car's charge, or a charger FTW can control."),
		"sonnen.lua": driver("sonnen", `"battery"`, true,
			"sonnenBatterie local JSON API v2: SoC + charge/discharge power. Read-only."),
		"zap.lua": driver("zap", `"meter", "pv", "battery"`, true,
			"P1/HAN site meter from a Sourceful Zap. Optionally read PV and battery telemetry from devices Zap already talks to. Prefer a native driver; use this when Zap is the only reader. The driver never writes."),
		"zaptec_cloud.lua": driver("zaptec_cloud", `"ev"`, true,
			"Zaptec Go / Go 2 / Pro via Zaptec Cloud. Email + password; optional charger serial."),
		"huawei.lua": driver("huawei", `"battery", "meter", "pv"`, true,
			"Huawei SUN2000 via Modbus TCP. Read-only until a held-zero command is verified on hardware."),
		// A controlling driver never carries a read-only reason, whatever its
		// description says.
		"easee_cloud.lua": driver("easee_cloud", `"ev"`, false,
			"Easee via Cloud. Read-only: this sentence must never reach the owner."),
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := LoadCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	want := map[string]string{
		"tesla_wall_connector": "Tesla offers no way for another system to set its current. Control goes through the car (tesla_vehicle) or a charger FTW can control, such as one on OCPP.",
		"tesla_cloud":          "Tesla accepts a command only when it is signed with a key paired to the car, and this driver cannot sign. Use tesla_vehicle (BLE) to start the car's charge, or a charger FTW can control.",
		"sonnen":               "",
		"zap":                  "",
		"zaptec_cloud":         "",
		"huawei":               "Read-only until a held-zero command is verified on hardware.",
		"easee_cloud":          "",
	}
	for _, e := range entries {
		if got := e.ReadOnlyReason; got != want[e.ID] {
			t.Errorf("%s ReadOnlyReason = %q, want %q", e.ID, got, want[e.ID])
		}
	}
	if got := readOnlyReason("Reads a car. read-only: the car takes no commands from this driver at all."); got != "The car takes no commands from this driver at all." {
		t.Errorf("a lowercase reason must start with a capital, got %q", got)
	}
	wall, ok := FindCatalogEntry(entries, "drivers/tesla_wall_connector.lua")
	if !ok || !wall.ReadOnly {
		t.Fatalf("FindCatalogEntry(tesla_wall_connector) = %+v, %v; want the read-only entry", wall, ok)
	}
	if !IsReadOnlyDriver(entries, "sonnen.lua") || IsReadOnlyDriver(entries, "easee_cloud.lua") {
		t.Error("IsReadOnlyDriver must follow FindCatalogEntry")
	}
	if _, ok := FindCatalogEntry(entries, ""); ok {
		t.Error("an empty lua path must match nothing")
	}
}
