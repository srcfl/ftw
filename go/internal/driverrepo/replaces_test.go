package driverrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/config"
)

// esphome-dsmr was folded into esphome_dsmr: a site that installed the old
// catalog entry moves to the release's driver instead of keeping a copy that
// no longer gets updates.
func TestDeviceOnARetiredDriverMovesToTheReleasesReplacement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ownFile   bool
		wantMoved bool
	}{
		{"moves", false, true},
		{"an operator's own file stays", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := newSupersedeSite(t, "1.0.3", "")
			old := []byte(strings.Replace(string(testDriver("1.0.3")), `id = "demo"`, `id = "demo-old"`, 1))
			site.channel.mu.Lock()
			site.channel.driver = old
			sum := sha256.Sum256(old)
			site.channel.manifest.Drivers[0].ID = "demo-old"
			site.channel.manifest.Drivers[0].Path = "drivers/demo-old.lua"
			site.channel.manifest.Drivers[0].Filename = "demo-old.lua"
			site.channel.manifest.Drivers[0].SHA256 = hex.EncodeToString(sum[:])
			site.channel.manifest.Drivers[0].URL = site.channelURL + "/demo.lua"
			site.channel.manifest.Drivers[0].HostAPI = components.CompatibleRange{Min: 1, Max: 1}
			site.channel.mu.Unlock()
			replacement := strings.Replace(string(testDriver("1.0.6")), "capabilities = { \"meter\" },",
				"capabilities = { \"meter\" },\n  replaces = { \"demo-old\" },", 1)
			if err := os.WriteFile(filepath.Join(site.bundled, "demo.lua"), []byte(replacement), 0o644); err != nil {
				t.Fatal(err)
			}
			manager, _ := site.boot("v0.137.0-beta.2")
			if err := manager.Refresh(context.Background(), "test"); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Install(context.Background(), "test", "demo-old", ""); err != nil {
				t.Fatal(err)
			}
			userDir := t.TempDir()
			if tc.ownFile {
				if err := os.WriteFile(filepath.Join(userDir, "demo-old.lua"), old, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{Drivers: []config.Driver{{Name: "p1", Lua: "drivers/demo-old.lua"}}}
			moved := manager.MigrateReplaced(cfg, userDir)
			if !tc.wantMoved {
				if len(moved) != 0 || cfg.Drivers[0].Lua != "drivers/demo-old.lua" {
					t.Fatalf("moved = %+v, lua = %s; the operator's own file must stay", moved, cfg.Drivers[0].Lua)
				}
				return
			}
			if len(moved) != 1 || moved[0].From != "demo-old" || moved[0].To != "demo" || cfg.Drivers[0].Lua != "drivers/demo.lua" {
				t.Fatalf("moved = %+v, lua = %s; want p1 on the release's drivers/demo.lua", moved, cfg.Drivers[0].Lua)
			}
			if active, _ := site.store.ActiveDriverRepoInstalls(); len(active) != 0 {
				t.Fatalf("the retired selection is still active: %+v", active)
			}
		})
	}
}
