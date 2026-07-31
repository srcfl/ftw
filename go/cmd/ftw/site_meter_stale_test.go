package main

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestEvaluateSiteDispatchFreshnessRejectsStaleMeter(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	reading := tel.Get("meter", telemetry.DerMeter)
	if reading == nil {
		t.Fatal("site meter reading missing")
	}

	got := evaluateSiteDispatchFreshnessAt(
		tel, "meter", 16, 3, time.Minute, reading.UpdatedAt.Add(time.Minute+time.Second),
	)
	if got.Reason != siteDispatchMeterStale {
		t.Fatalf("reason = %q, want %q", got.Reason, siteDispatchMeterStale)
	}
}

func TestEvaluateSiteDispatchFreshnessRejectsStalePhaseCurrents(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	metrics := []telemetry.MetricSnapshot{
		{Name: "meter_l1_a", Value: 8, UpdatedAt: now},
		{Name: "meter_l2_a", Value: 9, UpdatedAt: now.Add(-2 * time.Minute)},
		{Name: "meter_l3_a", Value: 7, UpdatedAt: now},
	}
	amps, available, fresh := phaseCurrentsFromSnapshots(metrics, 3, time.Minute, now)
	if !available {
		t.Fatal("phase telemetry should be available")
	}
	if fresh {
		t.Fatalf("phase telemetry unexpectedly fresh: %v", amps)
	}
}

func TestEvaluateSiteDispatchFreshnessPhaseRecovery(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	tel.EmitMetric("meter", "meter_l1_a", 8, "A", "", "")
	now := time.Now()

	blocked := evaluateSiteDispatchFreshnessAt(tel, "meter", 16, 3, time.Minute, now)
	if blocked.Reason != siteDispatchPhaseCurrentsStale {
		t.Fatalf("partial phase telemetry reason = %q, want %q",
			blocked.Reason, siteDispatchPhaseCurrentsStale)
	}

	tel.EmitMetric("meter", "meter_l2_a", 9, "A", "", "")
	tel.EmitMetric("meter", "meter_l3_a", 7, "A", "", "")
	tel.Update("meter", telemetry.DerMeter, 1100, nil, nil)
	recovered := evaluateSiteDispatchFreshnessAt(tel, "meter", 16, 3, time.Minute, time.Now())
	if !recovered.Allowed() {
		t.Fatalf("complete fresh phase telemetry remained blocked: %q", recovered.Reason)
	}
}

func TestEvaluateSiteDispatchFreshnessAllowsMeterWithoutPhaseTelemetry(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	now := time.Now()

	got := evaluateSiteDispatchFreshnessAt(tel, "meter", 16, 3, time.Minute, now)
	if !got.Allowed() {
		t.Fatalf("meter without optional phase telemetry blocked: %q", got.Reason)
	}
}

func TestStaleSiteDefaultTrackerIncludesOwnerOnceAndRearmsOnRecovery(t *testing.T) {
	var tracker staleSiteDefaultTracker
	names := []string{"ferroamp", "sungrow", "easee"}

	pending, entered, recovered := tracker.update(true, "ferroamp", names)
	if !entered || recovered {
		t.Fatalf("first stale transition: entered=%v recovered=%v", entered, recovered)
	}
	assertStringsEqual(t, pending, names)

	pending, entered, recovered = tracker.update(true, "ferroamp", names)
	if len(pending) != 0 || entered || recovered {
		t.Fatalf("steady stale state must not spam defaults: pending=%v entered=%v recovered=%v",
			pending, entered, recovered)
	}

	pending, _, _ = tracker.update(true, "ferroamp", append(names, "new-battery"))
	assertStringsEqual(t, pending, []string{"new-battery"})

	if _, _, recovered = tracker.update(false, "ferroamp", names); !recovered {
		t.Fatal("fresh transition was not reported")
	}
	pending, entered, _ = tracker.update(true, "ferroamp", names)
	if !entered {
		t.Fatal("tracker did not re-arm after recovery")
	}
	assertStringsEqual(t, pending, names)
}

func assertStringsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
