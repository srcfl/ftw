package mpc

import (
	"context"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A config reload that lands while a replan runs must not give that plan the
// old fuse for its slot grid limits and the new fuse for its battery clamp,
// or mix old and new export prices. The next replan uses the new values.
func TestReplanKeepsSiteEconomicsFromItsStart(t *testing.T) {
	svc := newCancellationTestService(t, nil)
	tel := telemetry.NewStore()
	soc := 0.5
	tel.Update("bat", telemetry.DerBattery, 0, &soc, nil)
	tel.DriverHealthMut("bat").RecordSuccess()
	svc.Tele = tel
	svc.UpdateBatteryFleet([]BatteryFleetMember{
		{Driver: "bat", CapacityWh: 10000, MaxChargeW: 5000, MaxDischargeW: 5000},
	}, 10000, 5000, 5000)
	svc.UpdateSiteEconomics(SiteEconomics{FuseMaxW: 11000, ExportBonusOreKwh: 10})
	reloaded := false
	// ForecastSnapshot runs mid-replan, after the grid limits were read.
	svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
		if !reloaded {
			reloaded = true
			svc.UpdateSiteEconomics(SiteEconomics{FuseMaxW: 2000, ExportBonusOreKwh: 99})
		}
		return ForecastInputs{}
	}

	check := func(reason string, fuseW, chargeW, bonus float64) {
		t.Helper()
		if svc.ReplanWithReason(context.Background(), reason) == nil {
			t.Fatalf("%s: no plan published", reason)
		}
		svc.mu.RLock()
		p, slots := svc.lastParams, svc.lastSlots
		svc.mu.RUnlock()
		if len(slots) == 0 || slots[0].Limits.MaxImportW != fuseW {
			t.Fatalf("%s: slot import limit=%v, want %v", reason, slots[0].Limits.MaxImportW, fuseW)
		}
		if p.MaxChargeW != chargeW || p.MaxDischargeW != chargeW {
			t.Fatalf("%s: battery clamp=%v/%v, want %v from the same fuse as the slots",
				reason, p.MaxChargeW, p.MaxDischargeW, chargeW)
		}
		if p.ExportBonusOreKwh != bonus {
			t.Fatalf("%s: export bonus=%v, want %v", reason, p.ExportBonusOreKwh, bonus)
		}
	}
	check("during-reload", 11000, 5000, 10)
	check("after-reload", 2000, 2000, 99)
}
