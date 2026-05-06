package drivers

import (
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
