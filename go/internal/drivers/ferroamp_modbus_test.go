package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// TestFerroampModbusLoads exercises telemetry and the read-only command
// boundary in drivers/ferroamp_modbus.lua.
func TestFerroampModbusLoads(t *testing.T) {
	tel := telemetry.NewStore()
	env := NewHostEnv("ferroamp_modbus", tel).WithModbus(&mockModbus{})
	d, err := NewLuaDriver("../../../drivers/ferroamp_modbus.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := d.Init(ctx, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	for _, action := range []string{"battery", "curtail", "curtail_disable", "deinit"} {
		cmd, _ := json.Marshal(map[string]any{"action": action, "power_w": 1000.0})
		if err := d.Command(ctx, cmd); !errors.Is(err, ErrReadOnlyDriver) {
			t.Fatalf("%s cmd: got %v, want read-only refusal", action, err)
		}
	}

	if err := d.DefaultMode(); err != nil {
		t.Fatalf("default_mode: %v", err)
	}
}

// TestFerroampModbusCatalogEntry verifies the DRIVER metadata block
// parses cleanly and advertises the correct id / capabilities. Distinct
// id from the existing "ferroamp" driver is the contract that lets both
// ship side-by-side.
func TestFerroampModbusCatalogEntry(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	var found *CatalogEntry
	for i, e := range entries {
		if e.ID == "ferroamp-modbus" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("ferroamp-modbus not in catalog (got %d entries)", len(entries))
	}
	if found.Manufacturer != "Ferroamp" {
		t.Errorf("manufacturer: got %q want Ferroamp", found.Manufacturer)
	}
	if !found.ReadOnly || !found.ReadOnlyDeclared {
		t.Error("ferroamp-modbus must declare its telemetry-only boundary")
	}
	wantProtocols := map[string]bool{"modbus": true}
	for _, p := range found.Protocols {
		if !wantProtocols[p] {
			t.Errorf("unexpected protocol %q", p)
		}
	}
	wantCaps := map[string]bool{"meter": true, "pv": true, "battery": true}
	for _, c := range found.Capabilities {
		if !wantCaps[c] {
			t.Errorf("unexpected capability %q", c)
		}
	}
}
