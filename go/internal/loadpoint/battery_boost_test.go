package loadpoint

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func newBatteryBoostController(t *testing.T, surplusOnly bool) (*Controller, *Manager, *EVSample) {
	t.Helper()
	mgr := NewManager()
	mgr.Load([]Config{{
		ID: "garage", DriverName: "charger", PluginSoC: 0.4,
		MinChargeW: 1380, MaxChargeW: 11000, SurplusOnly: surplusOnly,
	}})
	sample := &EVSample{Connected: true, RequestActive: true}
	mgr.Observe("garage", true, 0, 0, true)
	ctrl := NewController(
		mgr,
		func(now time.Time) (Directive, bool) {
			return Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute)}, true
		},
		func(string) (EVSample, bool) { return *sample, true },
		func(context.Context, string, []byte) error { return nil },
	)
	return ctrl, mgr, sample
}

func validBatteryBoost(now time.Time) BatteryBoostLease {
	return BatteryBoostLease{
		StartedAt: now, ExpiresAt: now.Add(time.Hour), MinBatterySoC: 0.3,
	}
}

func TestBatteryBoostEnableStatusCancelAndPersistence(t *testing.T) {
	ctrl, _, _ := newBatteryBoostController(t, false)
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	type save struct {
		lease   BatteryBoostLease
		cleared bool
	}
	var saves []save
	var stopped BatteryBoostStopReason
	ctrl.SetBatteryBoostSaver(func(_ string, lease BatteryBoostLease, cleared bool) {
		saves = append(saves, save{lease: lease, cleared: cleared})
	})
	ctrl.SetBatteryBoostStopped(func(_ string, reason BatteryBoostStopReason) { stopped = reason })

	status, err := ctrl.EnableBatteryBoost("garage", validBatteryBoost(now), now)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !status.Active || status.State != "active" || status.MinBatterySoC != 0.3 {
		t.Fatalf("active status = %+v", status)
	}
	if len(saves) != 1 || saves[0].cleared {
		t.Fatalf("enable persistence = %+v", saves)
	}

	status = ctrl.CancelBatteryBoost("garage", now.Add(time.Minute))
	if status.Active || status.StopReason != BatteryBoostStoppedCancelled {
		t.Fatalf("cancel status = %+v", status)
	}
	if len(saves) != 2 || !saves[1].cleared {
		t.Fatalf("cancel persistence = %+v", saves)
	}
	if stopped != BatteryBoostStoppedCancelled {
		t.Fatalf("stop hook reason = %q", stopped)
	}
}

func TestBatteryBoostValidationAndOperatorClamps(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name        string
		lease       BatteryBoostLease
		surplusOnly bool
	}{
		{"short duration", BatteryBoostLease{StartedAt: now, ExpiresAt: now.Add(30 * time.Second), MinBatterySoC: 0.3}, false},
		{"long duration", BatteryBoostLease{StartedAt: now, ExpiresAt: now.Add(MaxBatteryBoostDuration + time.Second), MinBatterySoC: 0.3}, false},
		{"low reserve", BatteryBoostLease{StartedAt: now, ExpiresAt: now.Add(time.Hour), MinBatterySoC: 0.04}, false},
		{"departure after expiry", BatteryBoostLease{StartedAt: now, ExpiresAt: now.Add(time.Hour), DepartureAt: now.Add(2 * time.Hour), MinBatterySoC: 0.3}, false},
		{"surplus only", validBatteryBoost(now), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl, _, _ := newBatteryBoostController(t, tc.surplusOnly)
			if _, err := ctrl.EnableBatteryBoost("garage", tc.lease, now); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// Charge now sets how much the car draws; a boost lets the home battery cover
// that draw. Starting either one keeps the other, and the boost counts what the
// held charge actually draws.
func TestBatteryBoostRunsWithChargeNow(t *testing.T) {
	now := time.Now()
	for _, order := range []string{"hold first", "boost first"} {
		t.Run(order, func(t *testing.T) {
			ctrl, mgr, _ := newBatteryBoostController(t, false)
			hold := func() { ctrl.SetManualHold("garage", ManualHold{Persistent: true, PowerW: 11000}) }
			if order == "hold first" {
				hold()
			}
			if _, err := ctrl.EnableBatteryBoost("garage", validBatteryBoost(now), now); err != nil {
				t.Fatalf("boost refused beside Charge now: %v", err)
			}
			if order == "boost first" {
				hold()
			}
			ctrl.TickWithDispatch(context.Background(), now.Add(time.Minute), true)
			if _, status := ctrl.BatteryBoost("garage", now.Add(time.Minute)); !status.Active {
				t.Fatalf("Charge now ended the boost: %+v", status)
			}
			mgr.Observe("garage", true, 11000, 0, true)
			st, _ := mgr.State("garage")
			if w, reserve := ctrl.ActiveBatteryBoostTotals([]State{st}, now.Add(time.Minute)); w != 11000 || reserve != 0.3 {
				t.Fatalf("boost covers %.0f W above %.2f, want the held 11000 W above 0.30", w, reserve)
			}
		})
	}
}

func TestBatteryBoostAutoStopsAndClearsPersistedLease(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		mutate     func(*Controller, *EVSample)
		tickAt     time.Time
		dispatchOK bool
		want       BatteryBoostStopReason
	}{
		{"expiry", nil, now.Add(time.Hour), true, BatteryBoostStoppedExpired},
		{"unplug", func(_ *Controller, s *EVSample) { s.Connected = false }, now.Add(time.Minute), true, BatteryBoostStoppedVehicleUnplugged},
		{"site safety", nil, now.Add(time.Minute), false, BatteryBoostStoppedSiteSafety},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl, _, sample := newBatteryBoostController(t, false)
			clears := 0
			ctrl.SetBatteryBoostSaver(func(_ string, _ BatteryBoostLease, cleared bool) {
				if cleared {
					clears++
				}
			})
			if _, err := ctrl.EnableBatteryBoost("garage", validBatteryBoost(now), now); err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(ctrl, sample)
			}
			ctrl.TickWithDispatch(context.Background(), tc.tickAt, tc.dispatchOK)
			_, status := ctrl.BatteryBoost("garage", tc.tickAt)
			if status.Active || status.StopReason != tc.want {
				t.Fatalf("status = %+v, want stop %q", status, tc.want)
			}
			if clears != 1 {
				t.Fatalf("persisted clear count = %d, want 1", clears)
			}
		})
	}
}

func TestBatteryBoostSafetyAndEVTargetStop(t *testing.T) {
	now := time.Now()
	ctrl, mgr, _ := newBatteryBoostController(t, false)
	ctrl.SetBatteryBoostSafety(func(string, BatteryBoostLease) BatteryBoostStopReason {
		return BatteryBoostStoppedBatteryReserve
	})
	if _, err := ctrl.EnableBatteryBoost("garage", validBatteryBoost(now), now); err == nil || err.Error() != string(BatteryBoostStoppedBatteryReserve) {
		t.Fatalf("preflight err = %v", err)
	}

	ctrl.SetBatteryBoostSafety(nil)
	lease := validBatteryBoost(now)
	lease.EVTargetSoC = 0.40
	if _, err := ctrl.EnableBatteryBoost("garage", lease, now); err != nil {
		t.Fatal(err)
	}
	mgr.Observe("garage", true, 0, 0, true)
	ctrl.TickWithDispatch(context.Background(), now.Add(time.Second), true)
	_, status := ctrl.BatteryBoost("garage", now.Add(time.Second))
	if status.StopReason != BatteryBoostStoppedEVTargetReached {
		t.Fatalf("target stop status = %+v", status)
	}
}

func TestBatteryBoostRestoreRejectsUnboundedOrExpiredRows(t *testing.T) {
	ctrl, _, _ := newBatteryBoostController(t, false)
	now := time.Now()
	for _, lease := range []BatteryBoostLease{
		{StartedAt: now.Add(-5 * time.Hour), ExpiresAt: now.Add(time.Hour), MinBatterySoC: 0.3},
		{StartedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Second), MinBatterySoC: 0.3},
	} {
		if ctrl.RestoreBatteryBoost("garage", lease, now) {
			t.Fatalf("restored unsafe lease %+v", lease)
		}
		_, status := ctrl.BatteryBoost("garage", now)
		if status.StopReason != BatteryBoostStoppedRestartInvalid {
			t.Fatalf("restart status = %+v", status)
		}
	}
}

func TestBatteryBoostRestoreRequiresLiveSafetyPreflight(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		setup func(*Controller, *Manager)
		want  BatteryBoostStopReason
	}{
		{
			name: "vehicle unplugged",
			setup: func(ctrl *Controller, mgr *Manager) {
				ctrl.SetBatteryBoostSafety(func(string, BatteryBoostLease) BatteryBoostStopReason { return "" })
				mgr.Observe("garage", false, 0, 0, false)
			},
			want: BatteryBoostStoppedVehicleUnplugged,
		},
		{
			name: "battery unavailable",
			setup: func(ctrl *Controller, _ *Manager) {
				ctrl.SetBatteryBoostSafety(func(string, BatteryBoostLease) BatteryBoostStopReason {
					return BatteryBoostStoppedBatteryUnavailable
				})
			},
			want: BatteryBoostStoppedBatteryUnavailable,
		},
		{
			name:  "core safety evaluator missing",
			setup: func(*Controller, *Manager) {},
			want:  BatteryBoostStoppedRestartInvalid,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl, mgr, _ := newBatteryBoostController(t, false)
			tc.setup(ctrl, mgr)
			if ctrl.RestoreBatteryBoost("garage", validBatteryBoost(now), now) {
				t.Fatal("restored lease without passing live safety preflight")
			}
			_, status := ctrl.BatteryBoost("garage", now)
			if status.Active || status.StopReason != tc.want {
				t.Fatalf("restart status = %+v, want stopped reason %q", status, tc.want)
			}
		})
	}
}

func TestBatteryBoostRestoreActivatesAfterLiveSafetyPreflight(t *testing.T) {
	ctrl, _, _ := newBatteryBoostController(t, false)
	ctrl.SetBatteryBoostSafety(func(string, BatteryBoostLease) BatteryBoostStopReason { return "" })
	now := time.Now()
	if !ctrl.RestoreBatteryBoost("garage", validBatteryBoost(now), now) {
		t.Fatal("valid lease did not restore after live safety preflight")
	}
	_, status := ctrl.BatteryBoost("garage", now)
	if !status.Active || status.State != "active" {
		t.Fatalf("restart status = %+v, want active", status)
	}
}

func TestActiveBatteryBoostTotalsArePerLoadpointAndUseStrictestReserve(t *testing.T) {
	mgr := NewManager()
	mgr.Load([]Config{{ID: "a", DriverName: "a"}, {ID: "b", DriverName: "b"}, {ID: "plain", DriverName: "plain"}})
	for _, id := range []string{"a", "b", "plain"} {
		mgr.Observe(id, true, map[string]float64{"a": 2000, "b": 3000, "plain": 7000}[id], 0, true)
	}
	ctrl := NewController(mgr, nil, nil, nil)
	now := time.Now()
	for id, reserve := range map[string]float64{"a": 0.20, "b": 0.35} {
		lease := validBatteryBoost(now)
		lease.MinBatterySoC = reserve
		if _, err := ctrl.EnableBatteryBoost(id, lease, now); err != nil {
			t.Fatal(err)
		}
	}
	power, reserve := ctrl.ActiveBatteryBoostTotals(mgr.States(), now)
	if power != 5000 || reserve != 0.35 {
		t.Fatalf("totals = %.0f W, %.2f reserve; want 5000 W, 0.35", power, reserve)
	}
}

func TestBatteryBoostLeaseJSONRoundTripIsFraction(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	lease := BatteryBoostLease{
		StartedAt: now, ExpiresAt: now.Add(time.Hour),
		MinBatterySoC: 0.30, EVTargetSoC: 0.80,
	}
	raw, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["min_battery_soc_pct"]; ok {
		t.Fatalf("lease JSON still emits percent key: %s", raw)
	}
	if m["min_battery_soc"] != 0.30 {
		t.Fatalf("min_battery_soc = %v, want 0.30", m["min_battery_soc"])
	}

	var back BatteryBoostLease
	if err := json.Unmarshal([]byte(`{"started_at":"2026-08-22T12:00:00Z","expires_at":"2026-08-22T13:00:00Z","min_battery_soc_pct":30,"ev_target_soc_pct":80}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.MinBatterySoC != 0.30 || back.EVTargetSoC != 0.80 {
		t.Fatalf("legacy percent hydrate = %+v", back)
	}

	var overflow BatteryBoostLease
	if err := json.Unmarshal([]byte(`{"started_at":"2026-08-22T12:00:00Z","expires_at":"2026-08-22T13:00:00Z","min_battery_soc":1.02}`), &overflow); err != nil {
		t.Fatal(err)
	}
	if overflow.MinBatterySoC != 1 {
		t.Fatalf("1.02 overflow = %v, want 1 (not 0.0102)", overflow.MinBatterySoC)
	}
}
