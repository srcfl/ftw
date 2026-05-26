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

func TestLastReplanInfoReturnsPair(t *testing.T) {
	s, _ := buildTestService(t, 0, 0)
	s.lastReplanAt = time.Unix(1700000000, 0)
	s.lastReason = "reactive-pv"
	ts, r := s.LastReplanInfo()
	if ts.Unix() != 1700000000 || r != "reactive-pv" {
		t.Errorf("got (%v, %q)", ts, r)
	}
}
