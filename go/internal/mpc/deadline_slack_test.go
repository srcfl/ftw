package mpc

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// #1718: a goal with hours of slack must not start the car at full
// power merely because no executable plan exists right now.
// Car 30 % -> 80 % of 60 kWh needs ~33.3 kWh AC, ~3 h at 11 kW; deadline in
// 10 h, so ~7 h of slack.
func TestDeadlineReserveWaitsForLatestStart(t *testing.T) {
	for _, fault := range []string{"no plan", "failed replacement", "restored archive"} {
		t.Run(fault, func(t *testing.T) {
			now := time.Now()
			mgr := loadpoint.NewManager()
			cfg := loadpoint.Config{ID: "a", DriverName: "charger-a", VehicleCapacityWh: 60000, PluginSoC: .3,
				MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 4140, 6900, 11000}}
			mgr.Load([]loadpoint.Config{cfg})
			mgr.Observe(cfg.ID, true, 0, 0, true)
			mgr.SetCurrentSoC(cfg.ID, .3)
			mgr.SetTarget(cfg.ID, .8, now.Add(10*time.Hour))
			svc := &Service{LoadpointStates: mgr.States}
			// The accepted plan holds 0 W now (cheap hours are at night).
			zeroPlan := Plan{GeneratedAtMs: now.UnixMilli(), Actions: []Action{{SlotStartMs: now.UnixMilli(), SlotLenMin: 15,
				LoadpointPowerW: map[string]float64{"a": 0}, LoadpointMaxPowerW: map[string]float64{"a": 11000}}}}
			switch fault {
			case "failed replacement":
				svc.InstallPlan(zeroPlan, Params{}, cfg.ID)
				svc.failedReplanGeneration = svc.publishedReplanGeneration + 1
			case "restored archive":
				// RestoreDiagnostic sets s.last but never s.executionPlan.
				p := zeroPlan
				svc.last = &p
				svc.lastLoadpointID = cfg.ID
			}
			var commanded float64 = -1
			ctrl := loadpoint.NewController(mgr, func(at time.Time) (loadpoint.Directive, bool) {
				d, ok := svc.SlotDirectiveAt(at)
				return d.LoadpointDirective(), ok
			}, func(string) (loadpoint.EVSample, bool) {
				return loadpoint.EVSample{Connected: true, RequestActive: true}, true
			}, func(_ context.Context, _ string, payload []byte) error {
				var cmd struct {
					Action string  `json:"action"`
					PowerW float64 `json:"power_w"`
				}
				if err := json.Unmarshal(payload, &cmd); err != nil {
					return err
				}
				if cmd.Action == "ev_set_current" {
					commanded = cmd.PowerW
				}
				return nil
			})
			ctrl.SetFuseEVMax(func() (float64, bool) { return 11000, true })
			ctrl.TickWithDispatch(context.Background(), now, true)
			st, _ := mgr.State(cfg.ID)
			t.Logf("fault=%s commanded=%.0f W reason=%q", fault, commanded, st.CommandedReason)
			if commanded > 0 {
				t.Fatalf("fault=%s: car started at %.0f W (%q) with ~7 h of slack before deadline", fault, commanded, st.CommandedReason)
			}
		})
	}
}

// The reserve starts at the latest start: the need at full power plus one
// hourly slot before the deadline. A running reserve keeps a second slot, so
// one SoC step does not stop it and start it again a few minutes later.
func TestDeadlineReserveLatestStart(t *testing.T) {
	now := time.Now()
	base := deadlineGoalState(now, "a") // ~33.3 kWh at 11 kW: ~3.03 h.
	need := (base.TargetSoC - base.CurrentSoC) * base.VehicleCapacityWh / loadpoint.DefaultChargeEfficiency
	atFull := time.Duration(need / base.MaxChargeW * float64(time.Hour))
	for _, c := range []struct {
		name    string
		slack   time.Duration
		running bool
		want    bool
	}{
		{"inside the latest start", 59 * time.Minute, false, true},
		{"before the latest start", 61 * time.Minute, false, false},
		{"running, inside two slots", 119 * time.Minute, true, true},
		{"running, before two slots", 121 * time.Minute, true, false},
	} {
		st := base
		st.TargetTime = now.Add(atFull + c.slack)
		if c.running {
			st.CommandedKnown, st.CommandedW = true, 11000
		}
		svc := &Service{LoadpointStates: func() []loadpoint.State { return []loadpoint.State{st} }}
		d, _ := svc.SlotDirectiveAt(now)
		if got := d.LoadpointFallback["a"]; got != c.want {
			t.Errorf("%s: reserve %v, want %v (%+v)", c.name, got, c.want, d)
		}
	}
}

// A loadpoint with min_charge_w 0 and a step table takes its legal minimum
// from the steps, so it gets the reserve within its latest start, and the
// controller orders power.
func TestDeadlineReserveZeroMinWithSteps(t *testing.T) {
	now := time.Now()
	st := deadlineGoalState(now, "a")
	st.MinChargeW = 0
	st.TargetTime = now.Add(30 * time.Minute)
	svc := &Service{LoadpointStates: func() []loadpoint.State { return []loadpoint.State{st} }}
	d, ok := svc.SlotDirectiveAt(now)
	if !ok || !d.LoadpointFallback["a"] || d.LoadpointMaxPowerW["a"] != 11000 {
		t.Fatalf("no reserve for min 0 with steps: ok=%v %+v", ok, d)
	}

	mgr := loadpoint.NewManager()
	cfg := loadpoint.Config{ID: "a", DriverName: "charger-a", VehicleCapacityWh: 60000, PluginSoC: .3,
		MinChargeW: 0, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 4140, 6900, 11000}}
	mgr.Load([]loadpoint.Config{cfg})
	mgr.Observe(cfg.ID, true, 0, 0, true)
	mgr.SetCurrentSoC(cfg.ID, .3)
	mgr.SetTarget(cfg.ID, .8, now.Add(2*time.Hour))
	svc = &Service{LoadpointStates: mgr.States}
	var commanded float64 = -1
	ctrl := loadpoint.NewController(mgr, func(at time.Time) (loadpoint.Directive, bool) {
		d, ok := svc.SlotDirectiveAt(at)
		return d.LoadpointDirective(), ok
	}, func(string) (loadpoint.EVSample, bool) {
		return loadpoint.EVSample{Connected: true, RequestActive: true}, true
	}, func(_ context.Context, _ string, payload []byte) error {
		var cmd struct {
			Action string  `json:"action"`
			PowerW float64 `json:"power_w"`
		}
		if err := json.Unmarshal(payload, &cmd); err != nil {
			return err
		}
		if cmd.Action == "ev_set_current" {
			commanded = cmd.PowerW
		}
		return nil
	})
	ctrl.SetFuseEVMax(func() (float64, bool) { return 11000, true })
	ctrl.TickWithDispatch(context.Background(), now, true)
	if commanded <= 0 {
		st, _ := mgr.State(cfg.ID)
		t.Fatalf("min 0 loadpoint within its latest start got %.0f W (%q)", commanded, st.CommandedReason)
	}
}

// Two goal cars share one 11 kW EV allowance and no plan is in force. Each
// latest start counts every goal due by that car's deadline, so both goals
// are met when the allowance goes earliest deadline first, as
// freezeFuseBudget gives it, and also, for equal deadlines, when it is split
// evenly. SoC is read in whole percent, as cars report it.
func TestDeadlineReserveSharedAllowance(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name       string
		dueA, dueB time.Duration
		edf        bool
	}{
		{"same deadline, earliest first", 10 * time.Hour, 10 * time.Hour, true},
		{"same deadline, even split", 10 * time.Hour, 10 * time.Hour, false},
		{"b one hour later, earliest first", 10 * time.Hour, 11 * time.Hour, true},
		{"b far later, earliest first", 4*time.Hour + 30*time.Minute, 20 * time.Hour, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, b := deadlineGoalState(t0, "a"), deadlineGoalState(t0, "b")
			a.TargetTime, b.TargetTime = t0.Add(c.dueA), t0.Add(c.dueB)
			soc := map[string]float64{"a": .3, "b": .3}
			commanded := map[string]float64{}
			for now := t0; now.Before(b.TargetTime); now = now.Add(time.Minute) {
				var states []loadpoint.State
				for _, base := range []loadpoint.State{a, b} { // deadline order
					if !now.Before(base.TargetTime) {
						continue
					}
					cur := base
					cur.CurrentSoC = math.Floor(soc[base.ID]*100+1e-9) / 100
					cur.CommandedKnown, cur.CommandedW = true, commanded[base.ID]
					states = append(states, cur)
				}
				svc := &Service{LoadpointStates: func() []loadpoint.State { return states }}
				d, _ := svc.SlotDirectiveAt(now)
				var want []string
				for _, st := range states {
					if d.LoadpointFallback[st.ID] && d.LoadpointMaxPowerW[st.ID] > 0 && soc[st.ID] < st.TargetSoC {
						want = append(want, st.ID)
					}
				}
				clear(commanded)
				room := 11000.0
				for _, id := range want {
					w := min(d.LoadpointMaxPowerW[id], room)
					if !c.edf {
						w = min(d.LoadpointMaxPowerW[id], 11000/float64(len(want)))
					}
					room -= w
					commanded[id] = w
					soc[id] = min(.8, soc[id]+w/60*loadpoint.DefaultChargeEfficiency/60000)
				}
			}
			for _, id := range []string{"a", "b"} {
				if soc[id] < .8-1e-9 {
					t.Errorf("%s missed its goal: SoC %.3f, want 0.80", id, soc[id])
				}
			}
		})
	}
}
