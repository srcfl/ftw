package loadpoint

import (
	"context"
	"testing"
	"time"
)

func newBatteryBoostController(t *testing.T, surplusOnly bool) (*Controller, *Manager, *EVSample) {
	t.Helper()
	mgr := NewManager()
	mgr.Load([]Config{{
		ID: "garage", DriverName: "charger", PluginSoCPct: 40,
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
		StartedAt: now, ExpiresAt: now.Add(time.Hour), MinBatterySoCPct: 30,
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
	if !status.Active || status.State != "active" || status.MinBatterySoCPct != 30 {
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
		hold        bool
	}{
		{"short duration", BatteryBoostLease{StartedAt: now, ExpiresAt: now.Add(30 * time.Second), MinBatterySoCPct: 30}, false, false},
		{"long duration", BatteryBoostLease{StartedAt: now, ExpiresAt: now.Add(MaxBatteryBoostDuration + time.Second), MinBatterySoCPct: 30}, false, false},
		{"low reserve", BatteryBoostLease{StartedAt: now, ExpiresAt: now.Add(time.Hour), MinBatterySoCPct: 4}, false, false},
		{"departure after expiry", BatteryBoostLease{StartedAt: now, ExpiresAt: now.Add(time.Hour), DepartureAt: now.Add(2 * time.Hour), MinBatterySoCPct: 30}, false, false},
		{"surplus only", validBatteryBoost(now), true, false},
		{"manual hold", validBatteryBoost(now), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl, _, _ := newBatteryBoostController(t, tc.surplusOnly)
			if tc.hold {
				ctrl.SetManualHold("garage", ManualHold{Persistent: true, PowerW: 1380})
			}
			if _, err := ctrl.EnableBatteryBoost("garage", tc.lease, now); err == nil {
				t.Fatal("expected validation error")
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
		{"operator hold", func(c *Controller, _ *EVSample) {
			c.SetManualHold("garage", ManualHold{Persistent: true, PowerW: 1380})
		}, now.Add(time.Minute), true, BatteryBoostStoppedOperatorHold},
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
	lease.EVTargetSoCPct = 40
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
		{StartedAt: now.Add(-5 * time.Hour), ExpiresAt: now.Add(time.Hour), MinBatterySoCPct: 30},
		{StartedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Second), MinBatterySoCPct: 30},
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
	for id, reserve := range map[string]float64{"a": 20, "b": 35} {
		lease := validBatteryBoost(now)
		lease.MinBatterySoCPct = reserve
		if _, err := ctrl.EnableBatteryBoost(id, lease, now); err != nil {
			t.Fatal(err)
		}
	}
	power, reserve := ctrl.ActiveBatteryBoostTotals(mgr.States(), now)
	if power != 5000 || reserve != 0.35 {
		t.Fatalf("totals = %.0f W, %.2f reserve; want 5000 W, 0.35", power, reserve)
	}
}
