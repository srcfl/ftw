package drivers

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A PV-only Growatt answers its battery registers with 0. It used to emit a
// battery anyway, and Overview drew a second battery at 0 W and 0 % beside
// the site's real one (srcfl/ftw#1669). A hybrid still reports its battery.
func TestGrowattEmitsABatteryOnlyWhenOneAnswers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		regs    map[uint16]uint16
		battery bool
	}{
		{"PV only", map[uint16]uint16{2: 25000}, false},
		{"hybrid", map[uint16]uint16{2: 25000, 1010: 100, 1013: 480, 1014: 50}, true},
		{"hybrid at 0 %", map[uint16]uint16{2: 25000, 1013: 520}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := telemetry.NewStore()
			env := NewHostEnv("growatt", tel).WithModbus(&pixiiTestModbus{regs: tc.regs})
			d, err := NewLuaDriver(filepath.Join("..", "..", "..", "drivers", "growatt.lua"), env)
			if err != nil {
				t.Fatalf("load Growatt driver: %v", err)
			}
			t.Cleanup(d.Cleanup)
			if err := d.Init(context.Background(), nil); err != nil {
				t.Fatalf("init: %v", err)
			}
			if _, err := d.Poll(context.Background()); err != nil {
				t.Fatalf("poll: %v", err)
			}
			if pv := tel.Get("growatt", telemetry.DerPV); pv == nil || pv.RawW != -2500 {
				t.Fatalf("PV reading %+v, want -2500 W", pv)
			}
			bat := tel.Get("growatt", telemetry.DerBattery)
			if (bat != nil) != tc.battery {
				t.Fatalf("battery reading %+v, want present=%v", bat, tc.battery)
			}
		})
	}
}
