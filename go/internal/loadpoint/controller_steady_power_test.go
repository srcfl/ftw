package loadpoint

import (
	"context"
	"math"
	"testing"
	"time"
)

// Easee reports power only when it changes, so a car charging at a steady
// current carries an ever older power timestamp. On the home box Core took
// that as a stale charger and stood it down every three minutes all night.
// With a fresh site meter the charge must continue.
func TestSteadyChargingWithUnchangedPowerReadingKeepsCharging(t *testing.T) {
	start := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	cfgs := []Config{{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11000}}
	// One 15-minute slot planned at 6900 W.
	dir := &Directive{SlotStart: start, SlotEnd: start.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{"garage": 6900 * 0.25}}
	samples := map[string]EVSample{"easee": {Connected: true, RequestActive: true, SessionWh: 1000, PowerMaxAge: 3 * time.Minute}}
	c := newTestController(t, cfgs, dir, samples, sender)

	var reportedAt time.Time
	previous := 0.0
	started := false
	for seconds := 0; seconds < 12*60; seconds += 5 {
		now := start.Add(time.Duration(seconds) * time.Second)
		sample := samples["easee"]
		// The charger holds whole amps on three phases, so its power only
		// changes with the amp step; the vendor reports it only on change.
		charger := math.Round(previous/690) * 690
		if charger != sample.PowerW || reportedAt.IsZero() {
			reportedAt = now
		}
		sample.PowerW = charger
		sample.PowerAt = reportedAt
		sample.PowerUnavailable = now.Sub(reportedAt) > sample.PowerWindow() && sample.PowerW > 0
		samples["easee"] = sample

		c.TickWithDispatch(context.Background(), now, true)
		command, ok := lastSetCurrent(sender.calls)
		if !ok {
			t.Fatalf("no current command at %ds", seconds)
		}
		if command.power > 0 {
			started = true
		} else if started {
			st, _ := c.manager.State("garage")
			t.Fatalf("steady charge stopped at %ds with an unchanged power reading (reason %q)", seconds, st.CommandedReason)
		}
		previous = command.power
	}
	if !started {
		t.Fatal("charging never started")
	}
}
