package api

import (
	"os"
	"path/filepath"
	"testing"
)

// /api/drivers/catalog carries a driver's setup_hint as it stands, and a
// driver without one has no key at all.
func TestDriverCatalogPassesSetupHintThrough(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"sungrow.lua": "DRIVER = {\n  id = \"sungrow\",\n  name = \"Sungrow\",\n" +
			"  setup_hint = \"Turn on Modbus TCP in the inverter's settings.\",\n}\n",
		"plain.lua": "DRIVER = {\n  id = \"plain\",\n  name = \"Plain\",\n}\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, entries := readDriverCatalog(t, New(&Deps{DriverDir: dir}))
	byID := map[string]map[string]any{}
	for _, e := range entries {
		byID[e["id"].(string)] = e
	}
	if got := byID["sungrow"]["setup_hint"]; got != "Turn on Modbus TCP in the inverter's settings." {
		t.Fatalf("sungrow setup_hint = %v", got)
	}
	if _, ok := byID["plain"]["setup_hint"]; ok {
		t.Fatalf("plain has a setup_hint key: %v", byID["plain"])
	}
}
