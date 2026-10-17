package main

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A car plugged in reaches a streaming phone as a plan with its windows in the
// time a solve takes, wired the way the box wires it: the real planner with
// its production reactive check (10 s, 30 s between reactive replans), the
// loadpoint manager on the event bus, the plug-in replan, the feed and the
// handler. The plug-in lands right after a replan, inside the gap the
// reactive check would wait out. A viewer gets nothing.
func TestAPlugInReachesAStreamingPhoneAsAPlan(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Hour)
	var prices []state.PricePoint
	for i := range 6 {
		prices = append(prices, state.PricePoint{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(), SlotLenMin: 60,
			SpotOreKwh: float64(140 - i*20), TotalOreKwh: float64(190 - i*20), Source: "test", FetchedAtMs: now.UnixMilli(),
		})
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatal(err)
	}

	// A battery online with a fresh level, which the planner waits for.
	tel := telemetry.NewStore()
	soc := 0.5
	tel.Update("bat", telemetry.DerBattery, 0, &soc, nil)
	tel.DriverHealthMut("bat").RecordSuccess()
	svc := mpc.New(st, tel, "SE3", mpc.Params{
		Mode: mpc.ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5,
		CapacityWh: 10000, InitialSoC: 0.5, SoCMin: 0.1, SoCMax: 0.95,
		MaxChargeW: 3000, MaxDischargeW: 3000, ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.Horizon = 5 * time.Hour
	svc.BaseLoad = 500

	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "easee", DriverName: "easee", VehicleCapacityWh: 60000, MinChargeW: 4100, MaxChargeW: 11000}})
	deadline := len(prices)*4 - 1
	svc.Loadpoints = func(int) []*mpc.LoadpointSpec {
		var specs []*mpc.LoadpointSpec
		for _, lp := range mgr.States() {
			if lp.PluggedIn {
				specs = append(specs, &mpc.LoadpointSpec{
					ID: lp.ID, CapacityWh: 60000, Levels: 11, SoCMax: 1, InitialSoC: 0.3, PluggedIn: true,
					TargetSoC: 0.6, TargetSlotIdx: deadline, MaxChargeW: 11000, AllowedStepsW: []float64{0, 11000},
				})
			}
		}
		return specs
	}

	// The charger's driver is healthy, so the manager confirms a cable, and
	// its clock is the test's, so the cable's two seconds out pass at once.
	bus := events.NewBus()
	mgr.SetBus(bus)
	replanOnPlugIn(bus, svc)
	tel.DriverHealthMut("easee").RecordSuccess()
	bus.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{"easee": *tel.DriverHealth("easee")}, Now: time.Now()})
	var ahead atomic.Int64
	mgr.SetNowFn(func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) })
	mgr.Observe("easee", false, 0, 0, false)

	feed := appproto.NewPlanFeed(appproto.PlanSettle)
	svc.OnPlanChange(feed.Changed)
	chargers := &appPlanChargers{planner: svc, mgr: mgr}
	owner := streamingSession(t, chargers, feed, apiauth.RoleOwner)
	viewer := streamingSession(t, chargers, feed, apiauth.RoleViewer)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); svc.Stop() })
	svc.Start(ctx)
	first := owner.waitPlan(t, func(p appproto.Plan) bool {
		return len(p.Chargers) == 1 && !p.Chargers[0].PluggedIn && !p.Chargers[0].Pending && p.Rev > 0
	})
	if len(first.Chargers[0].Windows) != 0 {
		t.Fatalf("an unplugged car has windows: %+v", first.Chargers[0])
	}

	// The feed sends a change at once unless it pushed in the last settle;
	// let the start-up plan's settle pass, as any quiet moment would.
	time.Sleep(2 * appproto.PlanSettle)
	ahead.Store(int64(3 * time.Second))
	plugged := time.Now()
	mgr.Observe("easee", true, 0, 0, true)
	cable := owner.waitPlan(t, func(p appproto.Plan) bool { return len(p.Chargers) == 1 && p.Chargers[0].PluggedIn })
	tookCable := time.Since(plugged)
	// A push reads plan revision and charger windows separately. A solve
	// finishing between those reads can expose the new windows with the
	// preceding revision. Wait for the published revision as well as the
	// windows; the delivery deadline below still applies to that full result.
	got := owner.waitPlan(t, func(p appproto.Plan) bool {
		return p.Rev > first.Rev && len(p.Chargers) == 1 && p.Chargers[0].PluggedIn && !p.Chargers[0].Pending && len(p.Chargers[0].Windows) > 0
	})
	took := time.Since(plugged)
	t.Logf("plug-in to the cable on a phone: %v (pending %v); to the plan: %v, %d windows, %d Wh",
		tookCable, cable.Chargers[0].Pending, took, len(got.Chargers[0].Windows), got.Chargers[0].TotalWh)
	if tookCable > 200*time.Millisecond {
		t.Fatalf("the cable reached the phone %v after the plug-in", tookCable)
	}
	if took > appproto.PlanSettle+time.Second {
		t.Fatalf("the plan reached the phone %v after the plug-in", took)
	}
	if got.Rev <= first.Rev || got.Chargers[0].Outdated {
		t.Fatalf("pushed rev %d after %d, car %+v", got.Rev, first.Rev, got.Chargers[0])
	}

	// A viewer's session is on the same feed; give it the settle to be sure.
	time.Sleep(2 * appproto.PlanSettle)
	if n := viewer.plans(t); len(n) != 0 {
		t.Fatalf("a viewer got %d plans", len(n))
	}
}

type pushedSession struct {
	sent *sentFrames
}

// streamingSession is one phone that said hello and subscribed.
func streamingSession(t *testing.T, chargers appproto.PlanChargers, feed *appproto.PlanFeed, role string) pushedSession {
	t.Helper()
	sent := &sentFrames{}
	h, err := appproto.New(appproto.Config{
		Clock: appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
		Site:  emptySite{},
		// Not newAppBoxInfo: the box's zone is read once per process, and
		// a test that sets TZ later must still be the first to read it.
		Info:         appBoxInfo{id: "ftw-0001", build: "test", tz: "UTC"},
		Modes:        &appModes{},
		Plans:        &appPlans{planner: chargers.(*appPlanChargers).planner, ctrl: &control.State{}, ctrlMu: &sync.Mutex{}},
		PlanChargers: chargers,
		PlanFeed:     feed,
		Codec:        appuplink.Codec(),
		Sender:       sent,
		Caller:       apiauth.Caller{Kind: apiauth.KindApp, Role: role, Scopes: appproto.ScopesForRole(role)},
		Grants:       roleGrant(role),
		Commands:     appproto.NewCmdLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	for _, msg := range []struct {
		t string
		b any
	}{
		{appproto.MsgHello, appproto.Hello{Proto: appproto.ProtoRange{Min: 0, Max: appproto.ProtoMax}}},
		{appproto.MsgSub, appproto.Sub{Bucket: 512, Hz: 1}},
	} {
		b, err := appproto.Marshal(msg.b)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := appuplink.Codec().EncodeFrame(appproto.Frame{Lane: appproto.LaneControl, Bucket: 512, Envelope: appproto.Envelope{T: msg.t, B: b}})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Handle(context.Background(), frame); err != nil {
			t.Fatal(err)
		}
	}
	if !h.Subscribed() {
		t.Fatal("not streaming")
	}
	return pushedSession{sent: sent}
}

func (s pushedSession) plans(t *testing.T) []appproto.Plan {
	t.Helper()
	var out []appproto.Plan
	for _, raw := range s.sent.all() {
		f, err := appuplink.Codec().DecodeFrame(raw)
		if err != nil {
			t.Fatal(err)
		}
		if f.Envelope.T != appproto.MsgPlan {
			continue
		}
		var p appproto.Plan
		if err := appproto.Unmarshal(f.Envelope.B, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (s pushedSession) waitPlan(t *testing.T, match func(appproto.Plan) bool) appproto.Plan {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		for _, p := range s.plans(t) {
			if match(p) {
				return p
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no matching plan in %d pushed", len(s.plans(t)))
	return appproto.Plan{}
}

type emptySite struct{}

func (emptySite) Snapshot() appproto.Snapshot { return appproto.Snapshot{} }

type roleGrant string

func (r roleGrant) Grant() (appproto.Enrolment, bool) {
	return appproto.Enrolment{Role: string(r), Epoch: 1}, true
}

// While a newer plan is being made the box goes on acting on the plan it has,
// so a phone keeps its windows, marked pending, and no shortfall: a plan being
// remade says nothing about whether the car makes it.
func TestAPendingPlanKeepsTheWindowsTheBoxActsOn(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(15 * time.Minute)
	if err := st.SavePrices([]state.PricePoint{{Zone: "SE4", SlotTsMs: now.UnixMilli(), SlotLenMin: 60, SpotOreKwh: 50, TotalOreKwh: 100}}); err != nil {
		t.Fatal(err)
	}
	cloud := 0.0
	if err := st.SaveForecasts([]state.ForecastPoint{{SlotTsMs: now.UnixMilli(), SlotLenMin: 15, CloudCoverPct: &cloud, Source: "test", FetchedAtMs: now.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	svc := mpc.New(st, nil, "SE4", mpc.Params{Mode: mpc.ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5, CapacityWh: 10000, InitialSoC: .5, SoCMin: .1, SoCMax: .95, MaxChargeW: 3000, MaxDischargeW: 3000, ChargeEfficiency: .95, DischargeEfficiency: .95})
	svc.Horizon = time.Hour
	svc.InstallPlan(mpc.Plan{GeneratedAtMs: time.Now().UnixMilli(), Actions: []mpc.Action{{
		SlotStartMs: now.UnixMilli(), SlotLenMin: 60, LoadpointPowerW: map[string]float64{"easee": 6000},
	}}, LoadpointShortfallWh: map[string]float64{"easee": 1000}}, svc.Defaults, "easee")
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "easee", DriverName: "easee", VehicleCapacityWh: 60000}})
	mgr.Observe("easee", true, 0, 0, true)
	chargers := &appPlanChargers{planner: svc, mgr: mgr}

	before := chargers.PlanChargers(time.Now())[0]
	if before.Pending || len(before.Windows) != 1 || before.ShortfallWh == nil {
		t.Fatalf("before: %+v", before)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	svc.PV = func(time.Time, float64) float64 { once.Do(func() { close(entered) }); <-release; return 0 }
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	svc.RequestReplan("test")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the replan never reached the forecast")
	}
	during := chargers.PlanChargers(time.Now())[0]
	if !during.Pending || len(during.Windows) != 1 || during.Windows[0] != before.Windows[0] || during.ShortfallWh != nil {
		t.Fatalf("during: %+v", during)
	}
	if during.GoalRev != before.GoalRev {
		t.Fatal("the goal moved with the plan")
	}
}
