package loadpoint

import (
	"testing"
	"time"
)

// When a surplus_only loadpoint is offered power again but the charger still
// reports "not requesting current" (NCRQ) from our own earlier sub-floor
// pause, and there's no vehicle-API binding to wake (a bare CTEK), the
// controller must cycle the contactor to make the vehicle renegotiate —
// throttled to at most once per vehicleWakeCooldown, and only when the stop is
// genuinely self-induced.
func TestWallboxResumeKickDecision(t *testing.T) {
	sender := &fakeSender{}
	c := newTestController(t, []Config{{
		ID: "garage", DriverName: "ctek", SurplusOnly: true,
	}}, nil, nil, sender)

	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	const floorW = 4140

	// surplus offered + charger NCRQ + self-paused → kick.
	if !c.shouldKickWallboxForResume(now, "garage", true, false, true, floorW) {
		t.Fatal("expected a kick on resume from a self-induced NCRQ")
	}
	// within cooldown → no second kick.
	if c.shouldKickWallboxForResume(now.Add(time.Minute), "garage", true, false, true, floorW) {
		t.Error("must not kick again within vehicleWakeCooldown")
	}
	// after cooldown → kicks again.
	if !c.shouldKickWallboxForResume(now.Add(vehicleWakeCooldown+time.Second), "garage", true, false, true, floorW) {
		t.Error("must kick again after the cooldown elapses")
	}

	// Guards (each at a fresh, post-cooldown instant so the throttle is clear):
	base := now.Add(2 * time.Hour)
	// charger already requesting → not stuck, no kick.
	if c.shouldKickWallboxForResume(base, "garage", true, true, true, floorW) {
		t.Error("no kick when the charger is already requesting current")
	}
	// not self-paused (vehicle genuinely finished) → no kick.
	if c.shouldKickWallboxForResume(base.Add(time.Hour), "garage", true, false, false, floorW) {
		t.Error("no kick when the stop was not self-induced")
	}
	// offering 0 W (still below floor) → nothing to resume into, no kick.
	if c.shouldKickWallboxForResume(base.Add(2*time.Hour), "garage", true, false, true, 0) {
		t.Error("no kick when we are not offering power")
	}
	// not a surplus loadpoint → no kick.
	if c.shouldKickWallboxForResume(base.Add(3*time.Hour), "garage", false, false, true, floorW) {
		t.Error("no kick for a non-surplus loadpoint")
	}
}
