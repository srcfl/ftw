package mpc

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Run the real charger translator from the same service callback used by
// battery dispatch. No optimizer success, price or battery is fabricated.
func TestDeadlineRuntimeContinuesThroughMissingPlan(t *testing.T) {
	for _, fault := range []string{"no plan", "battery unavailable before goal provider", "prices unavailable", "failed replacement", "expired plan", "goal reached", "site stale", "manual stop", "power unavailable", "no safe cap", "unknown safe cap"} {
		t.Run(fault, func(t *testing.T) {
			now := time.Now()
			mgr := loadpoint.NewManager()
			cfg := loadpoint.Config{ID: "a", DriverName: "charger-a", VehicleCapacityWh: 60000, PluginSoC: .3,
				MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 4140, 6900, 11000}}
			mgr.Load([]loadpoint.Config{cfg})
			mgr.Observe(cfg.ID, true, 0, 0, true)
			mgr.SetCurrentSoC(cfg.ID, .3)
			mgr.SetTarget(cfg.ID, .8, now.Add(time.Second)) // Deliberately impossible.
			svc := shadowTestService(t)
			svc.LoadpointStates = mgr.States
			t.Cleanup(func() { svc.replanWG.Wait(); svc.shadowWG.Wait() })
			var providerCalled bool
			svc.Loadpoints = func(int) []*LoadpointSpec { providerCalled = true; return nil }
			switch fault {
			case "battery unavailable before goal provider":
				svc.Tele = telemetry.NewStore()
				svc.BatteryFleet = []BatteryFleetMember{{Driver: "missing", CapacityWh: 10000, MaxChargeW: 2000, MaxDischargeW: 2000}}
				if svc.Replan(context.Background()) != nil || providerCalled {
					t.Fatal("fixture did not fail before the optimizer's goal provider")
				}
			case "prices unavailable":
				svc.Zone = "no_prices"
				if svc.Replan(context.Background()) != nil {
					t.Fatal("fixture unexpectedly planned without prices")
				}
			case "failed replacement", "expired plan":
				generated := now
				if fault == "expired plan" {
					generated = now.Add(-2 * MaxPlanAge)
				}
				svc.InstallPlan(Plan{GeneratedAtMs: generated.UnixMilli(), Actions: []Action{{SlotStartMs: now.UnixMilli(), SlotLenMin: 15}}}, Params{}, cfg.ID)
				if fault == "failed replacement" {
					svc.failedReplanGeneration = svc.publishedReplanGeneration + 1
				}
			case "goal reached":
				mgr.SetCurrentSoC(cfg.ID, .8)
			}
			sample := loadpoint.EVSample{Connected: true, RequestActive: true, PowerUnavailable: fault == "power unavailable"}
			var commanded float64 = -1
			ctrl := loadpoint.NewController(mgr, func(at time.Time) (loadpoint.Directive, bool) {
				d, ok := svc.SlotDirectiveAt(at)
				return d.LoadpointDirective(), ok
			}, func(string) (loadpoint.EVSample, bool) { return sample, true }, func(_ context.Context, _ string, payload []byte) error {
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
			if fault != "no safe cap" {
				ctrl.SetFuseEVMax(func() (float64, bool) { return 11000, fault != "unknown safe cap" })
			}
			if fault == "manual stop" {
				ctrl.SetManualHold(cfg.ID, loadpoint.ManualHold{PowerW: 0, Persistent: true})
			}
			ctrl.TickWithDispatch(context.Background(), now, fault != "site stale")
			wantCharge := fault != "goal reached" && fault != "site stale" && fault != "manual stop" && fault != "power unavailable" && fault != "no safe cap" && fault != "unknown safe cap"
			if (commanded > 0) != wantCharge || commanded < 0 || commanded > cfg.MaxChargeW {
				t.Fatalf("fault=%s commanded=%v wantCharge=%v", fault, commanded, wantCharge)
			}
			if wantCharge {
				st, _ := mgr.State(cfg.ID)
				if st.CommandedReason != "EV deadline reserve" {
					t.Fatalf("misleading reason: %s", st.CommandedReason)
				}
			}
			if fault != "failed replacement" && fault != "expired plan" && svc.Latest() != nil {
				t.Fatal("runtime reserve was published as an optimizer plan")
			}
		})
	}
}

func TestDeadlineReserveSmallRemainderLeavesRoomForOtherGoal(t *testing.T) {
	now := time.Now()
	a, b := deadlineGoalState(now, "a"), deadlineGoalState(now, "b")
	a.CurrentSoC = a.TargetSoC - loadpoint.DefaultChargeEfficiency/a.VehicleCapacityWh // 1 AC Wh.
	// Within the latest start.
	a.TargetTime = now.Add(30 * time.Minute)
	svc := &Service{LoadpointStates: func() []loadpoint.State { return []loadpoint.State{a, b} }}
	d, ok := svc.SlotDirectiveAt(now)
	if !ok || math.Abs(d.LoadpointEnergyWh["a"]-1) > 1e-6 || d.LoadpointMaxPowerW["a"] != a.MinChargeW || d.LoadpointMaxPowerW["b"] != b.MaxChargeW {
		t.Fatalf("small remainder took the whole fuse: %+v", d)
	}
}

func TestDeadlineGoalSnapshotLockOrder(t *testing.T) {
	now := time.Now()
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "a", DriverName: "charger-a", VehicleCapacityWh: 60000, PluginSoC: .3, MinChargeW: 1380, MaxChargeW: 11000}})
	mgr.Observe("a", true, 0, 0, true)
	mgr.SetTarget("a", .8, now.Add(time.Hour))
	var controlMu sync.Mutex
	svc := &Service{}
	ctrl := loadpoint.NewController(mgr, func(at time.Time) (loadpoint.Directive, bool) {
		d, ok := svc.SlotDirectiveAt(at)
		return d.LoadpointDirective(), ok
	}, func(string) (loadpoint.EVSample, bool) {
		return loadpoint.EVSample{Connected: true, RequestActive: true}, true
	}, nil)
	svc.LoadpointStates = func() []loadpoint.State {
		states := mgr.States()
		for _, st := range states {
			_, _ = ctrl.GetManualHold(st.ID, now)
		}
		return states
	}
	ctrl.SetFuseEVMax(func() (float64, bool) {
		controlMu.Lock()
		defer controlMu.Unlock()
		_ = mgr.States()
		return 11000, true
	})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			controlMu.Lock()
			_, _ = svc.SlotDirectiveAt(now)
			controlMu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			ctrl.Tick(context.Background(), now)
		}
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("goal snapshot inverted control/manager/hold locks")
	}
}
