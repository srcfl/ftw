package drivers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setup_hint is one sentence a driver gives the owner before FTW can read the
// device, such as turning on Modbus TCP. It is optional: a driver without it
// loads exactly as before, and the catalog JSON leaves the key out.
func TestLoadCatalogReadsSetupHint(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"hinted.lua": "DRIVER = {\n  id = \"hinted\",\n  name = \"Hinted\",\n" +
			"  protocols = { \"modbus\" },\n" +
			"  setup_hint = \"  Turn on Modbus TCP in the inverter's settings.  \",\n}\n",
		"plain.lua": "DRIVER = {\n  id = \"plain\",\n  name = \"Plain\",\n}\n",
		// Signed artifacts assign the table through a local, as
		// tools/ftw_repository.py in device-drivers writes it.
		"signed.lua": "DRIVER = { id = \"old\" }\n" +
			"local __sourceful_ftw_metadata = {\n  id = \"signed\",\n  name = \"Signed\",\n" +
			"  setup_hint = \"Turn on the local API in the app.\",\n}\n" +
			"DRIVER = __sourceful_ftw_metadata\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := LoadCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	byID := map[string]CatalogEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	if got := byID["hinted"].SetupHint; got != "Turn on Modbus TCP in the inverter's settings." {
		t.Fatalf("hinted SetupHint = %q", got)
	}
	if got := byID["signed"].SetupHint; got != "Turn on the local API in the app." {
		t.Fatalf("signed SetupHint = %q", got)
	}
	plain, ok := byID["plain"]
	if !ok || plain.SetupHint != "" {
		t.Fatalf("a driver without setup_hint must load with none: %+v", plain)
	}
	raw, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "setup_hint") {
		t.Fatalf("catalog JSON names setup_hint for a driver without one: %s", raw)
	}
	raw, _ = json.Marshal(byID["hinted"])
	if !strings.Contains(string(raw), `"setup_hint":"Turn on Modbus TCP in the inverter's settings."`) {
		t.Fatalf("catalog JSON lacks the hint: %s", raw)
	}
}

// Every bundled driver still loads now that the catalog reads another key,
// whether or not its DRIVER block has one.
func TestBundledDriversLoadWithOrWithoutSetupHint(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "drivers")
	matches, _ := filepath.Glob(filepath.Join(dir, "*.lua"))
	if len(matches) == 0 {
		t.Skip("bundled drivers are not fetched here; run make drivers")
	}
	entries, err := LoadCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if len(entries) != len(matches) {
		t.Fatalf("catalog has %d entries for %d driver files", len(entries), len(matches))
	}
	for _, e := range entries {
		if e.ID == "" {
			t.Errorf("%s lost its identity", e.Filename)
		}
		if strings.Contains(e.SetupHint, "\n") {
			t.Errorf("%s: setup_hint must be one line, got %q", e.Filename, e.SetupHint)
		}
	}
}
