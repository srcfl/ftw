package drivers

import (
	"os"
	"path/filepath"
	"strings"
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

func TestBundledDriverCatalogDocumentationIsComplete(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..")
	entries, err := LoadCatalog(filepath.Join(repoRoot, "drivers"))
	if err != nil {
		t.Fatalf("load bundled catalog: %v", err)
	}
	doc, err := os.ReadFile(filepath.Join(repoRoot, "docs", "driver-catalog.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		needle := "`" + filepath.ToSlash(entry.Path) + "`"
		if !strings.Contains(string(doc), needle) {
			t.Errorf("docs/driver-catalog.md is missing %s (%s)", entry.Name, needle)
		}
	}
}
