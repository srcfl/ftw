package loadpoint

import (
	"testing"
	"time"
)

// The manual status follows one Charge now press on an Easee tick by tick:
// sent, taken by the charger, charging — or not drawing, stalled, limited.
// Every branch the manual tab renders is pinned here.
func TestManualStatusFrom(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 22, 0, 0, 0, time.UTC)
	hold := ManualHold{PowerW: 11040, Persistent: true, StartedAt: t0} // 16 A × 3 × 230 V
	base := State{Phases: 3, VoltageV: 230}
	ordered := func(w float64, reason string, at time.Time) State {
		st := base
		st.CommandedW = w
		st.CommandedKnown = true
		st.CommandedReason = reason
		st.CommandedSinceMs = at.UnixMilli()
		return st
	}
	easee := func(limitA float64, charging bool, reason string, stalled bool) ChargerReading {
		return ChargerReading{Known: true, LimitA: limitA, LimitKnown: true, Charging: charging, Reason: reason, Stalled: stalled}
	}

	for _, tc := range []struct {
		name      string
		st        State
		ch        ChargerReading
		now       time.Time
		wantState string
		wantCmdA  float64
		wantLimit string
	}{
		{
			name: "no reading yet, previous automatic order still in the snapshot",
			st:   ordered(0, "no_plan_budget", t0.Add(-time.Hour)), ch: ChargerReading{},
			now: t0.Add(2 * time.Second), wantState: ManualSent, wantCmdA: 16,
		},
		{
			name: "charger still shows the old limit",
			st:   ordered(11040, "manual_hold", t0.Add(2*time.Second)), ch: easee(6, false, "car not drawing current", false),
			now: t0.Add(10 * time.Second), wantState: ManualSent, wantCmdA: 16,
		},
		{
			name: "charger took the limit, car has not started",
			st:   ordered(11040, "manual_hold", t0.Add(2*time.Second)), ch: easee(16, false, "car not drawing current", false),
			now: t0.Add(20 * time.Second), wantState: ManualAccepted, wantCmdA: 16,
		},
		{
			name: "power flows",
			st: func() State {
				st := ordered(11040, "manual_hold", t0.Add(2*time.Second))
				st.CurrentPowerW = 10800
				return st
			}(), ch: easee(16, true, "", false),
			now: t0.Add(30 * time.Second), wantState: ManualCharging, wantCmdA: 16,
		},
		{
			name: "charger took the limit, car still not drawing after the grace period",
			st:   ordered(11040, "manual_hold", t0.Add(2*time.Second)), ch: easee(16, false, "EV not accepting current", false),
			now: t0.Add(3 * time.Minute), wantState: ManualNotDrawing, wantCmdA: 16,
		},
		{
			name: "driver reports the command stalled",
			st:   ordered(11040, "manual_hold", t0.Add(2*time.Second)), ch: easee(16, false, "EV not accepting current", true),
			now: t0.Add(45 * time.Second), wantState: ManualStalled, wantCmdA: 16,
		},
		{
			name: "charger never reflected the limit",
			st:   ordered(11040, "manual_hold", t0.Add(2*time.Second)), ch: easee(6, false, "", false),
			now: t0.Add(4 * time.Minute), wantState: ManualStalled, wantCmdA: 16,
		},
		{
			name: "main fuse clamps the hold",
			st:   ordered(6900, "fuse_limit", t0.Add(40*time.Second)), ch: easee(10, false, "", false),
			now: t0.Add(50 * time.Second), wantState: ManualLimited, wantCmdA: 10, wantLimit: "fuse_limit",
		},
		{
			name: "fuse cooldown pauses the hold",
			st:   ordered(0, "fuse_cooldown", t0.Add(40*time.Second)), ch: easee(0, false, "", false),
			now: t0.Add(50 * time.Second), wantState: ManualLimited, wantCmdA: 0, wantLimit: "fuse_cooldown",
		},
		{
			name: "a charger without a limit reading is only ever sent or charging",
			st:   ordered(11040, "manual_hold", t0.Add(2*time.Second)), ch: ChargerReading{Known: true},
			now: t0.Add(time.Minute), wantState: ManualSent, wantCmdA: 16,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ManualStatusFrom(hold, true, tc.st, tc.ch, tc.now)
			if !got.Active {
				t.Fatal("status must be active while a hold is held")
			}
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q (%+v)", got.State, tc.wantState, got)
			}
			if got.CommandedA != tc.wantCmdA {
				t.Errorf("commanded_a = %v, want %v", got.CommandedA, tc.wantCmdA)
			}
			if got.LimitReason != tc.wantLimit {
				t.Errorf("limit_reason = %q, want %q", got.LimitReason, tc.wantLimit)
			}
			if got.RequestedA != 16 || got.RequestedW != 11040 {
				t.Errorf("requested = %v A / %v W, want 16 A / 11040 W", got.RequestedA, got.RequestedW)
			}
			if got.StartedAtMs != t0.UnixMilli() {
				t.Errorf("started_at_ms = %d, want %d", got.StartedAtMs, t0.UnixMilli())
			}
			if tc.ch.Reason != "" && got.ChargerReason != tc.ch.Reason {
				t.Errorf("charger_reason = %q, want %q", got.ChargerReason, tc.ch.Reason)
			}
		})
	}
}

func TestManualStatusFrom_SinceFollowsTheLatestOrder(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 22, 0, 0, 0, time.UTC)
	hold := ManualHold{PowerW: 11040, Persistent: true, StartedAt: t0}
	st := State{Phases: 3, VoltageV: 230, CommandedW: 11040, CommandedKnown: true, CommandedReason: "manual_hold",
		CommandedSinceMs: t0.Add(30 * time.Second).UnixMilli()}
	got := ManualStatusFrom(hold, true, st, ChargerReading{}, t0.Add(time.Minute))
	if got.SinceMs != t0.Add(30*time.Second).UnixMilli() {
		t.Errorf("since_ms = %d, want the order's time", got.SinceMs)
	}
	// An automatic order from before the hold does not move "since".
	st.CommandedReason = "no_plan_budget"
	st.CommandedSinceMs = t0.Add(-time.Hour).UnixMilli()
	got = ManualStatusFrom(hold, true, st, ChargerReading{}, t0.Add(time.Minute))
	if got.SinceMs != t0.UnixMilli() {
		t.Errorf("since_ms = %d, want the hold's start", got.SinceMs)
	}
}

func TestManualStatusFrom_InactiveIsEmpty(t *testing.T) {
	got := ManualStatusFrom(ManualHold{}, false, State{}, ChargerReading{}, time.Now())
	if got != (ManualStatus{}) {
		t.Errorf("inactive status must be the zero value, got %+v", got)
	}
}

func TestManualStatusUnavailableDoesNotReuseChargingPower(t *testing.T) {
	now := time.Now()
	hold := ManualHold{PowerW: 11040, Persistent: true, StartedAt: now.Add(-time.Minute)}
	st := State{Phases: 3, VoltageV: 230, CurrentPowerW: 10800}
	reading := ChargerReading{Known: true, Unavailable: true, Charging: true, UpdatedAt: now.Add(-5 * time.Minute)}
	got := ManualStatusFrom(hold, true, st, reading, now)
	if got.State != ManualUnavailable {
		t.Fatalf("old power became current charging: %+v", got)
	}
	if got.ChargerUpdatedAtMs != reading.UpdatedAt.UnixMilli() {
		t.Fatalf("missing age: %+v", got)
	}
	reading.Unavailable = false
	reading.UpdatedAt = now
	if got := ManualStatusFrom(hold, true, st, reading, now); got.State != ManualCharging {
		t.Fatalf("did not recover: %+v", got)
	}
}
