package mpc

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// buildTestService spins up a minimal Service with one cached plan
// covering the current time, so checkDivergence has something to
// compare against.
func buildTestService(t *testing.T, planPV, planLoad float64) (*Service, *telemetry.Store) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tel := telemetry.NewStore()
	tel.DriverHealthMut("site").RecordSuccess()
	tel.DriverHealthMut("inverter").RecordSuccess()

	s := &Service{
		Store:            st,
		Tele:             tel,
		Zone:             "SE3",
		SiteMeter:        "site",
		ReactiveInterval: 10 * time.Millisecond,
		MinReplanGap:     time.Millisecond,
		PVDivergenceWh:   500,
		LoadDivergenceWh: 400,
	}
	now := time.Now()
	s.last = &Plan{
		GeneratedAtMs: now.Add(-time.Minute).UnixMilli(),
		Actions: []Action{{
			SlotStartMs: now.Add(-5 * time.Minute).UnixMilli(),
			SlotLenMin:  15,
			PVW:         planPV,
			LoadW:       planLoad,
		}},
	}
	s.lastReplanAt = time.Now().Add(-time.Hour)
	return s, tel
}

// driveTicks runs N simulated checkDivergence ticks with the given
// per-tick spacing so the leaky integral can accumulate realistic
// energy. Each tick reuses current telemetry; caller sets it first.
func driveTicks(s *Service, n int, tickS float64) {
	// Prime lastTickMs so the first call has a non-zero dt.
	s.lastTickMs = time.Now().Add(-time.Duration(tickS * float64(time.Second))).UnixMilli()
	for i := 0; i < n; i++ {
		s.checkDivergence(context.Background())
		if s.lastReason == "reactive-pv" || s.lastReason == "reactive-load" {
			return
		}
		// Advance lastTickMs to simulate time passing without sleeping.
		s.lastTickMs = time.Now().Add(-time.Duration(tickS*float64(time.Second))).UnixMilli() -
			int64((float64(i+1))*tickS*1000)
	}
}

func TestReactivePVDivergenceIntegrates(t *testing.T) {
	// Plan expects 0 W PV. Actual is a sustained −3000 W generation
	// (i.e. twin predicted zero but panels are producing 3 kW).
	// 3000 W × 15 min = 750 Wh drift — past the 500 Wh threshold.
	s, tel := buildTestService(t, 0, 500)
	tel.Update("inverter", telemetry.DerPV, -3000, nil, nil)
	tel.Update("site", telemetry.DerMeter, -2500, nil, nil) // load 500 + pv -3000
	// 90 ticks × 10s = 900s = 15 min. Half-life is 900s too so EMA
	// reaches ~50% of steady-state = 0.5 × 3000 W × 900 s × (1/3600)
	// = 375 Wh. Need longer to cross 500 Wh — or bigger power gap.
	tel.Update("inverter", telemetry.DerPV, -6000, nil, nil)
	tel.Update("site", telemetry.DerMeter, -5500, nil, nil)
	driveTicks(s, 60, 10)
	if s.lastReason != "reactive-pv" {
		t.Errorf("expected reactive-pv after sustained divergence, got %q (pvInt=%.0fWh)",
			s.lastReason, s.pvErrIntWh)
	}
}

func TestReactiveLoadDivergenceIntegrates(t *testing.T) {
	// Plan expects 500 W load. Actual load is ~3000 W — big EV charge.
	// 2500 W × 15 min ≈ 625 Wh > 400 Wh threshold.
	s, tel := buildTestService(t, 0, 500)
	tel.Update("site", telemetry.DerMeter, 3000, nil, nil) // pv=0, bat=0 → load=3000
	driveTicks(s, 60, 10)
	if s.lastReason != "reactive-load" {
		t.Errorf("expected reactive-load, got %q (loadInt=%.0fWh)", s.lastReason, s.loadErrIntWh)
	}
}

func TestReactiveSkipsOnBriefTransient(t *testing.T) {
	// Big power swing but for only 2 ticks (20 s). Energy ≈
	// 3000 W × 20 s / 3600 = 16.7 Wh — far below threshold.
	s, tel := buildTestService(t, -5000, 1000)
	s.lastReason = "scheduled"
	tel.Update("inverter", telemetry.DerPV, -8000, nil, nil)
	tel.Update("site", telemetry.DerMeter, -7000, nil, nil)
	driveTicks(s, 2, 10)
	if s.lastReason != "scheduled" {
		t.Errorf("brief transient should not trigger; got %q", s.lastReason)
	}
}

func TestReactiveRespectsCooldown(t *testing.T) {
	s, tel := buildTestService(t, 0, 500)
	s.MinReplanGap = 60 * time.Second
	s.lastReplanAt = time.Now().Add(-10 * time.Second)
	s.lastReason = "scheduled"
	tel.Update("inverter", telemetry.DerPV, -8000, nil, nil)
	tel.Update("site", telemetry.DerMeter, -7500, nil, nil)
	driveTicks(s, 60, 10)
	if s.lastReason != "scheduled" {
		t.Errorf("cooldown should suppress replan; got %q", s.lastReason)
	}
}

func TestReactivePVDivergenceIgnoresOfflinePVReading(t *testing.T) {
	s, tel := buildTestService(t, 0, 500)
	s.LoadDivergenceWh = 0 // isolate the PV signal
	s.lastReason = "scheduled"

	tel.Update("inverter", telemetry.DerPV, -8000, nil, nil)
	tel.DriverHealthMut("inverter").SetOffline()
	tel.Update("site", telemetry.DerMeter, 500, nil, nil)
	driveTicks(s, 60, 10)

	if s.lastReason != "scheduled" {
		t.Errorf("offline PV reading must not trigger reactive replan, got %q", s.lastReason)
	}
}

func TestReactiveLoadDivergenceIgnoresOfflineBatteryReading(t *testing.T) {
	s, tel := buildTestService(t, 0, 500)
	s.PVDivergenceWh = 0 // isolate the load signal
	s.lastReason = "scheduled"

	tel.Update("site", telemetry.DerMeter, 500, nil, nil)
	tel.Update("bat-offline", telemetry.DerBattery, -4000, nil, nil)
	tel.DriverHealthMut("bat-offline").SetOffline()
	driveTicks(s, 60, 10)

	if s.lastReason != "scheduled" {
		t.Errorf("offline battery reading must not inflate load divergence, got %q", s.lastReason)
	}
}

func TestReactiveLoadDivergenceSkipsOfflineSiteMeter(t *testing.T) {
	s, tel := buildTestService(t, 0, 500)
	s.PVDivergenceWh = 0
	s.lastReason = "scheduled"

	tel.Update("site", telemetry.DerMeter, 5000, nil, nil)
	tel.DriverHealthMut("site").SetOffline()
	driveTicks(s, 60, 10)

	if s.lastReason != "scheduled" {
		t.Errorf("offline site meter must not drive load divergence, got %q", s.lastReason)
	}
}

// buildDefaultTestService spins up a Service with the production
// defaults (PV=250Wh, Load=200Wh, MinReplanGap=30s, 8-min half-life
// inside checkDivergence) so the tighter-trigger tests exercise what
// real deployments see.
func buildDefaultTestService(t *testing.T, planPV, planLoad float64) (*Service, *telemetry.Store) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tel := telemetry.NewStore()
	tel.DriverHealthMut("site").RecordSuccess()
	tel.DriverHealthMut("inverter").RecordSuccess()

	// Mirror New()'s defaults so this test exercises the production
	// reactive-trigger numbers.
	s := New(st, tel, "SE3", Params{})
	s.SiteMeter = "site"
	s.ReactiveInterval = 10 * time.Millisecond
	now := time.Now()
	s.last = &Plan{
		GeneratedAtMs: now.Add(-time.Minute).UnixMilli(),
		Actions: []Action{{
			SlotStartMs: now.Add(-5 * time.Minute).UnixMilli(),
			SlotLenMin:  15,
			PVW:         planPV,
			LoadW:       planLoad,
		}},
	}
	s.lastReplanAt = time.Now().Add(-time.Hour)
	return s, tel
}

// TestReactiveReplanFiresAtTighterLoadThreshold proves the lowered
// 200 Wh load threshold + 8-min half-life trip a replan after a few
// minutes of sustained drift, while the old 400 Wh + 15-min combo
// would still be coasting.
func TestReactiveReplanFiresAtTighterLoadThreshold(t *testing.T) {
	s, tel := buildDefaultTestService(t, 0, 500)
	// Sustained +1500 W house load drift. Steady-state of the leaky
	// integral with the new 480 s half-life is roughly
	// 1500 W × 480 s / ln(2) / 3600 ≈ 290 Wh — well past 200 Wh.
	tel.Update("site", telemetry.DerMeter, 2000, nil, nil) // pv=0, bat=0 → load=2000, plan=500, gap=1500
	driveTicks(s, 30, 10)
	if s.lastReason != "reactive-load" {
		t.Fatalf("expected reactive-load with new 200 Wh threshold, got %q (loadInt=%.0fWh)",
			s.lastReason, s.loadErrIntWh)
	}
}

// TestReactiveReplanFiresAtTighterPVThreshold mirrors the load case
// for PV — same magnitudes scaled to the 250 Wh threshold.
func TestReactiveReplanFiresAtTighterPVThreshold(t *testing.T) {
	s, tel := buildDefaultTestService(t, 0, 500)
	// Sustained PV undershoot: plan expected 0, actual −2000 W generation.
	// Same steady-state arithmetic — ~390 Wh > 250 Wh.
	tel.Update("inverter", telemetry.DerPV, -2000, nil, nil)
	tel.Update("site", telemetry.DerMeter, -1500, nil, nil) // load 500 + pv -2000
	driveTicks(s, 30, 10)
	if s.lastReason != "reactive-pv" {
		t.Fatalf("expected reactive-pv with new 250 Wh threshold, got %q (pvInt=%.0fWh)",
			s.lastReason, s.pvErrIntWh)
	}
}

// TestReactiveReplanRespectsShorterCooldown — the 30 s MinReplanGap
// still suppresses back-to-back replans even when the integral is hot.
func TestReactiveReplanRespectsShorterCooldown(t *testing.T) {
	s, tel := buildDefaultTestService(t, 0, 500)
	s.lastReplanAt = time.Now().Add(-5 * time.Second)
	s.lastReason = "scheduled"
	tel.Update("site", telemetry.DerMeter, 5000, nil, nil)
	driveTicks(s, 60, 10)
	if s.lastReason != "scheduled" {
		t.Fatalf("30 s cooldown should still suppress replan 5 s after the last; got %q", s.lastReason)
	}
}

// ---- Twin-drift trigger ----

// twinDriftService wires PV/Load predictors and a cached plan + planned
// predictions snapshot. The snapshot lives at fixed slot timestamps so
// the comparison is deterministic regardless of wall-clock drift.
func twinDriftService(t *testing.T) *Service {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tel := telemetry.NewStore()
	s := New(st, tel, "SE3", Params{})
	s.ReactiveInterval = 10 * time.Millisecond
	// Make sure cooldown doesn't suppress the first trigger.
	s.lastReplanAt = time.Now().Add(-time.Hour)
	return s
}

func TestTwinDriftReplanFiresOnLargePVShift(t *testing.T) {
	s := twinDriftService(t)

	// Snapshot the plan's PV predictions: 16 slots of 1000 W each.
	now := time.Now().Truncate(time.Minute)
	pp := &plannedPredictions{
		pv:        make([]float64, 16),
		load:      make([]float64, 16),
		slotStart: make([]time.Time, 16),
		builtAt:   now,
	}
	for i := 0; i < 16; i++ {
		pp.pv[i] = 1000
		pp.slotStart[i] = now.Add(time.Duration(i) * 15 * time.Minute)
	}
	s.plannedPredictions = pp
	// A live PV predictor that now returns 1500 W per slot — RMSE = 500 W,
	// well past the 250 W threshold.
	s.PV = func(time.Time, float64) float64 { return 1500 }
	// Stub a minimal plan so replan() (called on trigger) doesn't panic —
	// it'll bail with "no prices available yet" but lastReason has been
	// set on the service before that call, which is what we assert.
	s.last = &Plan{GeneratedAtMs: now.UnixMilli()}

	s.checkTwinDrift(context.Background())
	if s.lastReason != "twin-drift-pv" {
		t.Fatalf("expected twin-drift-pv after 500 W RMSE shift, got %q", s.lastReason)
	}
}

func TestTwinDriftReplanFiresOnLargeLoadShift(t *testing.T) {
	s := twinDriftService(t)

	now := time.Now().Truncate(time.Minute)
	pp := &plannedPredictions{
		pv:        make([]float64, 16),
		load:      make([]float64, 16),
		slotStart: make([]time.Time, 16),
		builtAt:   now,
	}
	for i := 0; i < 16; i++ {
		pp.load[i] = 800
		pp.slotStart[i] = now.Add(time.Duration(i) * 15 * time.Minute)
	}
	s.plannedPredictions = pp
	s.Load = func(time.Time) float64 { return 1100 } // RMSE = 300 W > 200 W
	s.last = &Plan{GeneratedAtMs: now.UnixMilli()}

	s.checkTwinDrift(context.Background())
	if s.lastReason != "twin-drift-load" {
		t.Fatalf("expected twin-drift-load after 300 W RMSE shift, got %q", s.lastReason)
	}
}

func TestTwinDriftReplanIgnoresTinyShift(t *testing.T) {
	s := twinDriftService(t)
	now := time.Now().Truncate(time.Minute)
	pp := &plannedPredictions{
		pv:        make([]float64, 16),
		load:      make([]float64, 16),
		slotStart: make([]time.Time, 16),
		builtAt:   now,
	}
	for i := 0; i < 16; i++ {
		pp.pv[i] = 1000
		pp.load[i] = 800
		pp.slotStart[i] = now.Add(time.Duration(i) * 15 * time.Minute)
	}
	s.plannedPredictions = pp
	// PV drifted only 100 W — under 250 W threshold. Load held.
	s.PV = func(time.Time, float64) float64 { return 1100 }
	s.Load = func(time.Time) float64 { return 800 }
	s.last = &Plan{GeneratedAtMs: now.UnixMilli()}
	s.lastReason = "scheduled"

	s.checkTwinDrift(context.Background())
	if s.lastReason != "scheduled" {
		t.Fatalf("100 W RMSE must not trigger replan, got %q", s.lastReason)
	}
}

func TestTwinDriftReplanRespectsCooldown(t *testing.T) {
	s := twinDriftService(t)
	s.MinReplanGap = 30 * time.Second
	s.lastReplanAt = time.Now().Add(-5 * time.Second) // inside cooldown

	now := time.Now().Truncate(time.Minute)
	pp := &plannedPredictions{
		pv:        make([]float64, 16),
		load:      make([]float64, 16),
		slotStart: make([]time.Time, 16),
		builtAt:   now,
	}
	for i := 0; i < 16; i++ {
		pp.pv[i] = 1000
		pp.slotStart[i] = now.Add(time.Duration(i) * 15 * time.Minute)
	}
	s.plannedPredictions = pp
	s.PV = func(time.Time, float64) float64 { return 2000 } // huge drift
	s.last = &Plan{GeneratedAtMs: now.UnixMilli()}
	s.lastReason = "scheduled"

	s.checkTwinDrift(context.Background())
	if s.lastReason != "scheduled" {
		t.Fatalf("cooldown should suppress twin-drift replan, got %q", s.lastReason)
	}
}

func TestTwinDriftReplanSkipsWhenPredictorIsNil(t *testing.T) {
	s := twinDriftService(t)
	// No predictors wired (s.PV == nil && s.Load == nil) and no snapshot.
	// Must not panic; lastReason untouched.
	s.lastReason = "scheduled"
	s.checkTwinDrift(context.Background())
	if s.lastReason != "scheduled" {
		t.Fatalf("nil predictors must not trigger replan, got %q", s.lastReason)
	}

	// Even with a snapshot present but predictors nil, no trigger.
	now := time.Now()
	pp := &plannedPredictions{
		pv:        []float64{1000, 1000, 1000},
		load:      []float64{500, 500, 500},
		slotStart: []time.Time{now, now.Add(15 * time.Minute), now.Add(30 * time.Minute)},
		builtAt:   now,
	}
	s.plannedPredictions = pp
	s.checkTwinDrift(context.Background())
	if s.lastReason != "scheduled" {
		t.Fatalf("nil predictors with snapshot must still not trigger, got %q", s.lastReason)
	}
}

func TestLastReplanInfoReturnsPair(t *testing.T) {
	s, _ := buildTestService(t, 0, 0)
	s.lastReplanAt = time.Unix(1700000000, 0)
	s.lastReason = "reactive-pv"
	ts, r := s.LastReplanInfo()
	if ts.Unix() != 1700000000 || r != "reactive-pv" {
		t.Errorf("got (%v, %q)", ts, r)
	}
}
