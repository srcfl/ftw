package loadpoint

import (
	"context"
	"testing"
	"time"
)

var graceCar = EVSample{Connected: true, RequestActive: true, PowerW: 4140, SessionWh: 6000, DeviceID: "dev-1", SessionID: "sess-1"}

// unplugFlap runs Charge now at 4140 W, then one unplugged reading, then
// the given connected reading 5 s later. It returns whether the hold is
// still there and the last order.
func unplugFlap(t *testing.T, unplugged, back EVSample) (bool, float64) {
	t.Helper()
	now := time.Now()
	cfg := holdLoadpoint()
	samples := map[string]EVSample{}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, &Directive{SlotStart: now, SlotEnd: now.Add(time.Hour)}, samples, sender)
	samples[cfg.DriverName] = graceCar
	c.Tick(context.Background(), now)
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true})
	c.Tick(context.Background(), now.Add(5*time.Second))
	if n := len(sender.calls); n == 0 || sender.calls[n-1].power != 4140 {
		t.Fatalf("Charge now not dispatched: %+v", sender.calls)
	}
	samples[cfg.DriverName] = unplugged
	c.Tick(context.Background(), now.Add(10*time.Second))
	samples[cfg.DriverName] = back
	c.Tick(context.Background(), now.Add(15*time.Second))
	_, held := c.GetManualHold(cfg.ID, now.Add(15*time.Second))
	last := -1.0
	if n := len(sender.calls); n > 0 {
		last = sender.calls[n-1].power
	}
	return held, last
}

// One Connected=false reading between two readings of the same device and
// session does not end Charge now; cars.UnplugGrace keeps that session 30 s.
func TestOneUnpluggedReadingKeepsChargeNow(t *testing.T) {
	held, last := unplugFlap(t, EVSample{Connected: false, DeviceID: "dev-1", SessionID: "sess-1"}, graceCar)
	if !held || last != 4140 {
		t.Fatalf("Charge now gone after one 5 s unplug reading of the same session: held=%v last order=%v W", held, last)
	}
}

// A charger that drops the session id while it reads unplugged still
// keeps Charge now when the same session comes back.
func TestUnpluggedReadingWithoutSessionKeepsChargeNow(t *testing.T) {
	held, last := unplugFlap(t, EVSample{Connected: false, DeviceID: "dev-1"}, graceCar)
	if !held || last != 4140 {
		t.Fatalf("Charge now gone after one unplug reading without a session id: held=%v last order=%v W", held, last)
	}
}

// Another session within the grace is another car: Charge now ends and the
// plan decides, as an unplug did before.
func TestOtherSessionAfterShortUnplugClearsChargeNow(t *testing.T) {
	other := graceCar
	other.SessionID = "sess-2"
	held, last := unplugFlap(t, EVSample{Connected: false, DeviceID: "dev-1"}, other)
	if held || last != 0 {
		t.Fatalf("a new session inherited Charge now: held=%v last order=%v W", held, last)
	}
}
