package mpc

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
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
	s.lastTickMs = time.Now().Add(-time.Duration(tickS*float64(time.Second))).UnixMilli()
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

func TestReactiveLoadDivergenceTriggersFasterInExpensiveSlot(t *testing.T) {
	// Plan expects 500 W load. Actual load is ~1000 W — a small 500 W
	// gap that at the default 400 Wh threshold takes ~48 minutes to
	// cross. In an expensive slot (4× horizon mean) the scaled
	// threshold is 100 Wh, crossed in ~12 minutes — fast enough that
	// the planner re-decides before the slot ends instead of letting
	// the wrong-side trade run for most of the slot.
	s, tel := buildTestService(t, 0, 500)
	// Anchor the existing test plan with a price 4× the horizon mean.
	s.last.Baselines = &Baselines{AvgPriceOre: 100}
	s.last.Actions[0].PriceOre = 400 // 4× mean → scale = 0.25 → 100 Wh load threshold
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	// 80 ticks × 10 s = 800 s. Half-life is 900 s, so EMA reaches
	// ~46% of steady-state = 0.46 × 500 W × 900 s × (1/3600) ≈ 58 Wh
	// at this rate — but the integral itself accumulates faster;
	// with a 100 Wh threshold the scaled trigger should fire well
	// before 80 ticks (vs the 400 Wh threshold which it would not
	// cross in this duration).
	driveTicks(s, 80, 10)
	if s.lastReason != "reactive-load" {
		t.Errorf("expensive-slot threshold should fire on 500 W sustained gap, got %q (loadInt=%.0fWh)",
			s.lastReason, s.loadErrIntWh)
	}
}

func TestReactiveLoadDivergenceHoldsInCheapSlot(t *testing.T) {
	// Same 500 W gap, but in a cheap slot (1/4 of horizon mean). The
	// scaled threshold is 1600 Wh (4× nominal), so the divergence
	// trigger stays quiet — there's no money to save by chasing the
	// forecast miss when the slot is essentially free.
	s, tel := buildTestService(t, 0, 500)
	s.last.Baselines = &Baselines{AvgPriceOre: 100}
	s.last.Actions[0].PriceOre = 25 // 0.25× mean → scale = 4.0 → 1600 Wh load threshold
	tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
	driveTicks(s, 80, 10)
	if s.lastReason == "reactive-load" {
		t.Errorf("cheap-slot threshold should suppress replan; fired anyway (loadInt=%.0fWh)",
			s.loadErrIntWh)
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
