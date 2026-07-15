package drivers

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type solisStringModbus struct {
	regs map[uint16][]uint16
}

func (m *solisStringModbus) Read(addr, count uint16, kind int32) ([]uint16, error) {
	src := m.regs[addr]
	if len(src) < int(count) {
		out := make([]uint16, count)
		copy(out, src)
		return out, nil
	}
	out := make([]uint16, count)
	copy(out, src[:count])
	return out, nil
}

func (m *solisStringModbus) WriteSingle(addr, value uint16) error { return nil }
func (m *solisStringModbus) WriteMulti(addr uint16, values []uint16) error {
	return nil
}
func (m *solisStringModbus) Close() error { return nil }

func TestSolisStringEmitsPVOnlyInSiteConvention(t *testing.T) {
	tel := telemetry.NewStore()
	env := NewHostEnv("solis-string", tel).WithModbus(&solisStringModbus{
		regs: map[uint16][]uint16{
			3004: {0, 4200, 0, 4400, 0, 12345},
			3021: {3501, 82, 3562, 79, 0, 0, 0, 0},
			3033: {2301, 0, 0, 183, 0, 0},
			3040: {1, 423, 5001, 1},
		},
	})
	d, err := NewLuaDriver("../../../drivers/solis_string.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	if err := d.Init(context.Background(), map[string]any{"serial": "SOLIS-STRING-42"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	make, sn := env.Identity()
	if make != "Ginlong Solis" || sn != "SOLIS-STRING-42" {
		t.Fatalf("identity = (%q, %q), want Ginlong Solis/SOLIS-STRING-42", make, sn)
	}

	pv := tel.Get("solis-string", telemetry.DerPV)
	if pv == nil {
		t.Fatal("expected pv telemetry")
	}
	if pv.RawW != -4200 {
		t.Fatalf("pv RawW = %v, want -4200", pv.RawW)
	}
	if got := tel.Get("solis-string", telemetry.DerBattery); got != nil {
		t.Fatalf("battery telemetry should not be emitted: %+v", got)
	}
	if got := tel.Get("solis-string", telemetry.DerMeter); got != nil {
		t.Fatalf("meter telemetry should not be emitted: %+v", got)
	}

	var data map[string]any
	if err := json.Unmarshal(pv.Data, &data); err != nil {
		t.Fatalf("pv data: %v", err)
	}
	if !near(data["mppt1_v"].(float64), 350.1) {
		t.Errorf("mppt1_v = %v, want 350.1", data["mppt1_v"])
	}
	if !near(data["mppt1_a"].(float64), 8.2) {
		t.Errorf("mppt1_a = %v, want 8.2", data["mppt1_a"])
	}
	if !near(data["dc_w"].(float64), 4400) {
		t.Errorf("dc_w = %v, want 4400", data["dc_w"])
	}
	if !near(data["lifetime_wh"].(float64), 12345000) {
		t.Errorf("lifetime_wh = %v, want 12345000", data["lifetime_wh"])
	}
	if !near(data["temp_c"].(float64), 42.3) {
		t.Errorf("temp_c = %v, want 42.3", data["temp_c"])
	}
	if !near(data["hz"].(float64), 50.01) {
		t.Errorf("hz = %v, want 50.01", data["hz"])
	}
}

func near(got, want float64) bool {
	return math.Abs(got-want) < 0.000001
}

func TestSolisStringCatalogEntry(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	var found *CatalogEntry
	for i, e := range entries {
		if e.ID == "solis-string" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("solis-string not in catalog")
	}
	if found.Manufacturer != "Ginlong Solis" {
		t.Errorf("manufacturer = %q, want Ginlong Solis", found.Manufacturer)
	}
	if len(found.Capabilities) != 1 || found.Capabilities[0] != "pv" {
		t.Errorf("capabilities = %v, want [pv]", found.Capabilities)
	}
}
