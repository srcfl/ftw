package control

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestJoinedNoBatteryNoPlanGoalContinuity(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	deadline := now.Add(3 * time.Hour)
	cfg := loadpoint.Config{ID: "garage", DriverName: "charger", VehicleCapacityWh: 60000, PluginSoC: .3, MinChargeW: 1380, MaxChargeW: 11000, PhaseMode: "3p", AllowedStepsW: []float64{0, 1380, 4140, 6900, 9660, 11000}}
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{cfg})
	mgr.Observe(cfg.ID, true, 0, 0, true)
	if !mgr.SetCurrentSoC(cfg.ID, .3) || !mgr.SetTarget(cfg.ID, .8, deadline) {
		t.Fatal("goal fixture rejected")
	}
	svc := &mpc.Service{LoadpointStates: mgr.States}
	dir, ok := svc.SlotDirectiveAt(now)
	if !ok || !dir.EVOnly || !dir.LoadpointFallback[cfg.ID] || svc.Latest() != nil {
		t.Fatalf("missing EV-only goal intent: %+v ok=%v", dir, ok)
	}
	tel := telemetry.NewStore()
	phaseA := 6000.0 / (3 * 230)
	meterData, _ := json.Marshal(map[string]float64{"l1_a": phaseA, "l2_a": phaseA, "l3_a": phaseA})
	tel.Update("meter", telemetry.DerMeter, 6000, nil, meterData)
	tel.RecordDriverSuccess("meter")
	tel.Update(cfg.DriverName, telemetry.DerEV, 0, nil, json.RawMessage(`{"connected":true,"request_active":true}`))
	tel.RecordDriverSuccess(cfg.DriverName)
	st := NewState(0, 0, "meter")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SiteFuseAmps = 25
	st.SiteFuseVoltage = 230
	st.SiteFusePhases = 3
	st.SlotDirective = func(at time.Time) (SlotDirective, bool) {
		d, ok := svc.SlotDirectiveAt(at)
		return SlotDirectiveFromMPC(d), ok
	}
	targets := ComputeDispatch(tel, st, nil, 17250)
	if len(targets) != 0 {
		t.Fatalf("no-battery fallback invented storage commands: %v", targets)
	}
	if !st.PlanStale {
		t.Fatal("EV-only intent falsely cleared missing battery-plan status")
	}
	var commands []float64
	controller := loadpoint.NewController(mgr,
		func(at time.Time) (loadpoint.Directive, bool) {
			d, ok := svc.SlotDirectiveAt(at)
			return d.LoadpointDirective(), ok
		},
		func(string) (loadpoint.EVSample, bool) {
			return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: 0, PowerAt: now}, true
		},
		func(_ context.Context, _ string, p []byte) error {
			var cmd struct {
				Action string  `json:"action"`
				PowerW float64 `json:"power_w"`
			}
			if err := json.Unmarshal(p, &cmd); err != nil {
				return err
			}
			if cmd.Action == "ev_set_current" {
				commands = append(commands, cmd.PowerW)
			}
			return nil
		})
	controller.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	controller.SetFuseEVMax(func() (float64, bool) { return EVFuseBudget(tel, st, mgr.States(), controller.FuseConfigs(), 17250) })
	controller.SetFusePhaseRoomA(func() (float64, bool) { return EVFusePhaseRoomA(st) })
	controller.TickWithDispatch(ctx, now, true)
	if len(commands) == 0 || commands[len(commands)-1] <= 0 {
		t.Fatalf("goal stopped at integration boundary: %v", commands)
	}
	got := commands[len(commands)-1]
	legal := false
	for _, w := range cfg.AllowedStepsW {
		if math.Abs(w-got) < 1e-6 {
			legal = true
		}
	}
	if !legal || 6000+got > 17250.001 || phaseA+got/(3*230) > st.fuseLineA()+.001 {
		t.Fatalf("unsafe or illegal EV offer=%v, grid=%v phaseA=%v", got, 6000+got, phaseA+got/(3*230))
	}
	if svc.Latest() != nil {
		t.Fatal("dispatch promoted live reserve into an accepted plan")
	}
	// The same saved goal now reaches its target; a later tick must release it.
	if !mgr.SetCurrentSoC(cfg.ID, .8) {
		t.Fatal("could not reach goal")
	}
	controller.TickWithDispatch(ctx, now.Add(5*time.Second), true)
	if commands[len(commands)-1] != 0 {
		t.Fatalf("reached goal kept charging: %v", commands)
	}
	state, ok := mgr.State(cfg.ID)
	if !ok || state.TargetSoC != .8 || !state.TargetTime.Equal(deadline) {
		t.Fatalf("controller erased durable goal intent: %+v", state)
	}
	t.Logf("no accepted plan, no battery: legal EV offer %.0fW at grid %.0fW, then0W at goal", got, 6000+got)
}

func TestJoinedMixedPhaseNoCreditBeforeMeasuredReduction(t *testing.T) {
	ctx := context.Background()
	wall := time.Now()
	cfgA := loadpoint.Config{ID: "early", DriverName: "charger-1p", VehicleCapacityWh: 60000, PluginSoC: .3, MinChargeW: 1380, MaxChargeW: 5750, PhaseMode: "1p", AllowedStepsW: []float64{0, 1380, 2300, 3680, 4600, 5750}}
	cfgB := loadpoint.Config{ID: "later", DriverName: "charger-3p", VehicleCapacityWh: 60000, PluginSoC: .3, MinChargeW: 4140, MaxChargeW: 11000, PhaseMode: "3p", AllowedStepsW: []float64{0, 4140, 6900, 8280, 9660, 11000}}
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{cfgA, cfgB})
	for _, cfg := range []loadpoint.Config{cfgA, cfgB} {
		mgr.Observe(cfg.ID, true, 0, 0, true)
		mgr.SetCurrentSoC(cfg.ID, .3)
	}
	mgr.SetTarget(cfgA.ID, .8, wall.Add(time.Hour))
	mgr.SetTarget(cfgB.ID, .8, wall.Add(3*time.Hour))
	svc := &mpc.Service{LoadpointStates: mgr.States}
	tel := telemetry.NewStore()
	st := NewState(0, 0, "meter")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SiteFuseAmps = 25
	st.SiteFuseVoltage = 230
	st.SiteFusePhases = 3
	st.SlotDirective = func(at time.Time) (SlotDirective, bool) {
		d, ok := svc.SlotDirectiveAt(at)
		return SlotDirectiveFromMPC(d), ok
	}
	actualA, actualB := 0.0, 11000.0
	sends := 0
	controller := loadpoint.NewController(mgr,
		func(at time.Time) (loadpoint.Directive, bool) {
			d, ok := svc.SlotDirectiveAt(at)
			return d.LoadpointDirective(), ok
		},
		func(driver string) (loadpoint.EVSample, bool) {
			w := actualA
			if driver == cfgB.DriverName {
				w = actualB
			}
			return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: w, PowerAt: time.Now(), SessionWhUnavailable: true}, true
		},
		func(_ context.Context, driver string, p []byte) error {
			var cmd struct {
				Action string  `json:"action"`
				PowerW float64 `json:"power_w"`
			}
			if err := json.Unmarshal(p, &cmd); err != nil {
				return err
			}
			if cmd.Action != "ev_set_current" {
				return nil
			}
			if driver == cfgA.DriverName {
				actualA = cmd.PowerW
			} else if driver == cfgB.DriverName {
				actualB = cmd.PowerW
			}
			sends++
			worst := actualA/230 + actualB/690
			if worst > 24.001 {
				t.Errorf("command%d: phase overage before sibling reduction is measured: A=%v B=%v L1=%.3fA", sends, actualA, actualB, worst)
			}
			return nil
		})
	controller.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	controller.SetFuseEVMax(func() (float64, bool) { return EVFuseBudget(tel, st, mgr.States(), controller.FuseConfigs(), 17250) })
	controller.SetFusePhaseRoomA(func() (float64, bool) { return EVFusePhaseRoomA(st) })
	emit := func(driver string, kind telemetry.DerType, at time.Time, w float64, amps bool) {
		d := map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano), "control_power_available": true}
		if amps {
			d["l1_a"] = actualA/230 + actualB/690
			d["l2_a"] = actualB / 690
			d["l3_a"] = actualB / 690
		}
		b, _ := json.Marshal(d)
		tel.Update(driver, kind, w, nil, b)
		tel.RecordDriverSuccess(driver)
	}
	for step := 0; step < 6; step++ {
		// Both source series bracket a real site sample. Commands are then applied
		// in configured order, so A can rise before B's reduction has taken effect.
		at := wall.Add(-9*time.Second + time.Duration(step)*1500*time.Millisecond)
		emit(cfgA.DriverName, telemetry.DerEV, at, actualA, false)
		emit(cfgB.DriverName, telemetry.DerEV, at, actualB, false)
		emit("meter", telemetry.DerMeter, at.Add(500*time.Millisecond), actualA+actualB, true)
		emit(cfgA.DriverName, telemetry.DerEV, at.Add(time.Second), actualA, false)
		emit(cfgB.DriverName, telemetry.DerEV, at.Add(time.Second), actualB, false)
		_ = ComputeDispatch(tel, st, nil, 17250)
		controller.TickWithDispatch(ctx, time.Now(), true)
	}
	if actualA <= 0 || actualB >= 11000 {
		t.Fatalf("earlier goal never gained safe measured headroom: A=%v B=%v", actualA, actualB)
	}
	t.Logf("%d real sends across6 ticks: final1p %.0fW +3p %.0fW, worst phase%.2fA", sends, actualA, actualB, actualA/230+actualB/690)
}

// Without a plan, a car whose own charger takes less than a stepless
// charger's minimum (a 3.7 kW plug-in hybrid on an 11 or 22 kW charger) is
// ordered that minimum, as the plan orders it: never 0 W because the car's
// own maximum is lower. On a table with one-phase and three-phase levels it
// is ordered the lowest three-phase level, not a one-phase level the
// three-phase order cannot run (review of #1807).
func TestJoinedNoPlanGoalSmallCarGetsChargerMinimum(t *testing.T) {
	ctx := context.Background()
	mixed := []float64{0}
	for _, phases := range []float64{1, 3} {
		for a := 6.0; a <= 16; a++ {
			mixed = append(mixed, a*230*phases)
		}
	}
	for _, ch := range []struct {
		name       string
		mode       string
		minW, maxW float64
		steps      []float64
		carMaxW    float64
	}{
		{"11 kW", "", 4140, 11040, nil, 3680},
		{"22 kW", "", 4140, 22080, nil, 3680},
		{"mixed table", "", 1380, 11040, mixed, 3680},
		{"mixed table, auto", "auto", 1380, 11040, mixed, 3680},
		{"mixed table, 3p", "3p", 1380, 11040, mixed, 3680},
		// The charger's minimum above the car's maximum, and that above
		// the lowest one-phase level: the car's own limits leave no level.
		{"mixed table, minimum 2,000 W, a 1.5 kW car", "", 2000, 11040, mixed, 1500},
	} {
		now := time.Now()
		cfg := loadpoint.Config{ID: "garage", DriverName: "charger", VehicleCapacityWh: 15000, PluginSoC: .3, PhaseMode: ch.mode,
			MinChargeW: ch.minW, MaxChargeW: ch.maxW, AllowedStepsW: ch.steps}
		mgr := loadpoint.NewManager()
		mgr.Load([]loadpoint.Config{cfg})
		mgr.Observe(cfg.ID, true, 0, 0, true)
		if !mgr.SetCarBinding(cfg.ID, loadpoint.CarBinding{ID: "phev", MaxW: ch.carMaxW}) ||
			!mgr.SetCurrentSoC(cfg.ID, .3) || !mgr.SetTarget(cfg.ID, .8, now.Add(3*time.Hour)) {
			t.Fatal("goal fixture rejected")
		}
		svc := &mpc.Service{LoadpointStates: mgr.States}
		tel := telemetry.NewStore()
		phaseA := 1000.0 / (3 * 230)
		meterData, _ := json.Marshal(map[string]float64{"l1_a": phaseA, "l2_a": phaseA, "l3_a": phaseA})
		tel.Update("meter", telemetry.DerMeter, 1000, nil, meterData)
		tel.RecordDriverSuccess("meter")
		tel.Update(cfg.DriverName, telemetry.DerEV, 0, nil, json.RawMessage(`{"connected":true,"request_active":true}`))
		tel.RecordDriverSuccess(cfg.DriverName)
		st := NewState(0, 0, "meter")
		st.Mode = ModePlannerArbitrage
		st.UseEnergyDispatch = true
		st.SiteFuseAmps = 25
		st.SiteFuseVoltage = 230
		st.SiteFusePhases = 3
		var commands []float64
		controller := loadpoint.NewController(mgr,
			func(at time.Time) (loadpoint.Directive, bool) {
				d, ok := svc.SlotDirectiveAt(at)
				return d.LoadpointDirective(), ok
			},
			func(string) (loadpoint.EVSample, bool) {
				return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: 0, PowerAt: now}, true
			},
			func(_ context.Context, _ string, p []byte) error {
				var cmd struct {
					Action string  `json:"action"`
					PowerW float64 `json:"power_w"`
				}
				if err := json.Unmarshal(p, &cmd); err != nil {
					return err
				}
				if cmd.Action == "ev_set_current" {
					commands = append(commands, cmd.PowerW)
				}
				return nil
			})
		controller.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
		svc.PlanningLimits = controller.PlanningLimits // as main.go wires it
		controller.SetFuseEVMax(func() (float64, bool) { return EVFuseBudget(tel, st, mgr.States(), controller.FuseConfigs(), 17250) })
		controller.SetFusePhaseRoomA(func() (float64, bool) { return EVFusePhaseRoomA(st) })
		controller.TickWithDispatch(ctx, now, true)
		if len(commands) == 0 || commands[len(commands)-1] != 4140 {
			t.Errorf("%s: orders %v, want 4140 W", ch.name, commands)
		}
	}
}
