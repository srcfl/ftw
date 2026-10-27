package loadpoint

import (
	"context"
	"testing"
	"time"
)

func surplusHoldFuse(t *testing.T, aConnected bool) (map[string]float64, string) {
	t.Helper()
	now := time.Date(2026, 10, 21, 20, 0, 0, 0, time.Local)
	steps := []float64{0, 4140, 5520, 6900, 8280, 9660, 11040}
	configs := []Config{
		{ID: "a", DriverName: "charger-a", SurplusOnly: true, MinChargeW: 4140, MaxChargeW: 11040, AllowedStepsW: steps, PhaseMode: "3p"},
		{ID: "b", DriverName: "charger-b", SurplusOnly: true, MinChargeW: 4140, MaxChargeW: 11040, AllowedStepsW: steps, PhaseMode: "3p"},
	}
	sender := &fakeSender{}
	c := newTestController(t, configs, &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute)},
		map[string]EVSample{"charger-a": {Connected: aConnected, RequestActive: aConnected}, "charger-b": {Connected: true, RequestActive: true}}, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetSiteSurplusForEV(func() (float64, bool) { return 0, true })
	c.SetFuseEVMax(func() (float64, bool) { return 8000, true })
	c.SetManualHold("b", ManualHold{PowerW: 11040, Persistent: true})
	c.Tick(context.Background(), now)
	got := map[string]float64{}
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			got[cmd.driver] = cmd.power
		}
	}
	st, _ := c.manager.State("b")
	t.Logf("orders %v, b reason %q, frozen caps %v", got, st.CommandedReason, c.tickFuseCaps)
	return got, st.CommandedReason
}

// Two solar-only chargers, no PV surplus, EV fuse cap 8 kW. Charge now on b
// must get a share of the cap; idle solar-only a orders 0 W.
func TestChargeNowOnSurplusOnlyGetsFuseShare(t *testing.T) {
	got, reason := surplusHoldFuse(t, true)
	if got["charger-b"] <= 0 {
		t.Fatalf("Charge now on b got %v W (%q) while a ordered %v W; cap 8000 W", got["charger-b"], reason, got["charger-a"])
	}
}

// Control: with a unplugged, Charge now on b charges.
func TestChargeNowOnSurplusOnlyAlone(t *testing.T) {
	got, reason := surplusHoldFuse(t, false)
	if got["charger-b"] <= 0 {
		t.Fatalf("b alone got %v W (%q)", got["charger-b"], reason)
	}
}
