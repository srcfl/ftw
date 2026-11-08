package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestPlanOff(t *testing.T) {
	for _, c := range []struct {
		planned, actual float64
		off             bool
	}{
		{2595, -1231, true}, // the other way
		{-1000, 300, true},
		{839, 50, true},
		{500, 300, false}, // under the floor
		{4000, 3000, false},
		{4000, 2700, true}, // over 30 % of the plan
		{0, 250, false},    // idle plan, small drift
		{0, 400, true},
		{150, -50, false}, // both within the idle band's reach, small gap
	} {
		if got := planOff(c.planned, c.actual); got != c.off {
			t.Errorf("planOff(%v, %v) = %t, want %t", c.planned, c.actual, got, c.off)
		}
	}
}

func TestPlanNowTrackerHoldsAMinute(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.Local)
	var tr planNowTracker
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	feed := func(s int, off bool) string {
		status, _, _, _ := tr.observe(planNowSample{at: at(s)}, off)
		return status
	}
	if got := feed(0, false); got != planNowAsPlanned {
		t.Fatalf("first reading = %q", got)
	}
	// A 30 s cloud changes nothing.
	for s := 5; s <= 30; s += 5 {
		if got := feed(s, true); got != planNowAsPlanned {
			t.Fatalf("at %d s = %q", s, got)
		}
	}
	feed(35, false)
	// A minute that holds does, dated from when it began.
	for s := 40; s < 95; s += 5 {
		if got := feed(s, true); got != planNowAsPlanned {
			t.Fatalf("at %d s = %q", s, got)
		}
	}
	_, _, settles, _ := tr.observe(planNowSample{at: at(95)}, true)
	if !settles.Equal(at(100)) {
		t.Fatalf("settles at %v, want %v", settles, at(100))
	}
	status, since, settles, _ := tr.observe(planNowSample{at: at(100)}, true)
	if status != planNowOffPlan || !since.Equal(at(40)) || !settles.IsZero() {
		t.Fatalf("after a minute: %q since %v, settles %v", status, since, settles)
	}
	// Coming back holds a minute too.
	if got := feed(105, false); got != planNowOffPlan {
		t.Fatalf("back on plan at once: %q", got)
	}
	for s := 110; s <= 160; s += 5 {
		feed(s, false)
	}
	if got := feed(165, false); got != planNowAsPlanned {
		t.Fatalf("after a minute back: %q", got)
	}
}

func TestPlanNowTrackerAfterAGapStartsOver(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.Local)
	var tr planNowTracker
	tr.observe(planNowSample{at: t0}, false)
	status, since, _, _ := tr.observe(planNowSample{at: t0.Add(31 * time.Second)}, true)
	if status != planNowOffPlan || !since.Equal(t0.Add(31*time.Second)) {
		t.Fatalf("after a gap: %q since %v", status, since)
	}
}

func TestPlanNowTrackerCountsTwoReadsOnce(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.Local)
	var tr planNowTracker
	tr.observe(planNowSample{at: t0, battery: 100}, false)
	_, _, _, m := tr.observe(planNowSample{at: t0.Add(200 * time.Millisecond), battery: 900}, false)
	if m.battery != 100 {
		t.Fatalf("mean = %v, want the first reading alone", m.battery)
	}
}

func TestPlanNowTrackerMeansTheLastMinute(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.Local)
	var tr planNowTracker
	tr.observe(planNowSample{at: t0, battery: 9000}, false)
	var m planNowSample
	for s := 5; s <= 90; s += 5 {
		_, _, _, m = tr.observe(planNowSample{at: t0.Add(time.Duration(s) * time.Second), battery: 1000}, false)
	}
	if m.battery != 1000 {
		t.Fatalf("mean = %v, the old reading should be gone", m.battery)
	}
}

func TestPlanNowCause(t *testing.T) {
	pv := func(w float64) *float64 { return &w }
	for _, c := range []struct {
		name string
		plan PlanNow
		m    planNowSample
		want string
	}{
		{"8 November: less sun than the forecast", PlanNow{PlannedBatteryW: 2595, PlannedPVW: -3814, PlannedLoadW: 1219, PVW: pv(0), LoadW: pv(0)},
			planNowSample{battery: -1231, pv: -1392, load: 1230}, "less_solar"},
		{"more sun", PlanNow{PlannedBatteryW: -1000, PlannedPVW: -1000, PlannedLoadW: 2000, PVW: pv(0), LoadW: pv(0)},
			planNowSample{battery: 500, pv: -2500, load: 2000}, "more_solar"},
		{"the home draws more", PlanNow{PlannedBatteryW: 2000, PlannedPVW: -3000, PlannedLoadW: 1000, PVW: pv(0), LoadW: pv(0)},
			planNowSample{battery: 500, pv: -3000, load: 2500}, "more_home"},
		{"the home draws less", PlanNow{PlannedLoadW: 2000, PlannedPVW: -2000, PVW: pv(0), LoadW: pv(0)},
			planNowSample{battery: 1500, pv: -2000, load: 500}, "less_home"},
		{"the car draws more", PlanNow{PlannedBatteryW: 3000, PlannedPVW: -4000, PlannedLoadW: 1000, PVW: pv(0), LoadW: pv(0)},
			planNowSample{battery: 0, pv: -4000, load: 1000, ev: 3000}, "more_car"},
		{"the car draws less", PlanNow{PlannedEVW: 3000, PlannedPVW: -4000, PlannedLoadW: 1000, PVW: pv(0), LoadW: pv(0)},
			planNowSample{battery: 3000, pv: -4000, load: 1000}, "less_car"},
		{"nothing measured explains it", PlanNow{PlannedBatteryW: 3000, PlannedPVW: -3000, PlannedLoadW: 1000, PVW: pv(0), LoadW: pv(0)},
			planNowSample{battery: 500, pv: -3000, load: 1000}, "other"},
		{"a flow without a reading is left out", PlanNow{PlannedBatteryW: 2595, PlannedPVW: -3814, PlannedLoadW: 1219, LoadW: pv(0)},
			planNowSample{battery: -1231, pv: 0, load: 1230}, "other"},
	} {
		plan := c.plan
		if got := planNowCause(&plan, c.m); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// planNowSite is a site whose plan means to charge 2.6 kW on 3.8 kW of sun
// while the battery covers the home on 1.4 kW.
func planNowSite(t *testing.T, batteryW float64) (*Server, *control.State) {
	t.Helper()
	now := time.Now()
	tel := telemetry.NewStore()
	soc := 0.5
	tel.Update("sungrow", telemetry.DerMeter, 20, nil, nil)
	tel.Update("sungrow", telemetry.DerPV, -1392, nil, nil)
	tel.Update("sungrow", telemetry.DerBattery, batteryW, &soc, nil)
	tel.RecordDriverSuccess("sungrow")
	ctrl := control.NewState(0, 42, "sungrow")
	ctrl.Mode = control.ModePlannerArbitrage
	svc := &mpc.Service{}
	svc.InstallPlan(mpc.Plan{GeneratedAtMs: now.UnixMilli(), Actions: []mpc.Action{{
		SlotStartMs: now.Add(-time.Minute).UnixMilli(), SlotLenMin: 15,
		BatteryW: 2595, PVW: -3814, LoadW: 1219, SoC: 0.5,
	}}}, mpc.Params{Mode: mpc.ModeArbitrage, CapacityWh: 9600, ChargeEfficiency: 1}, "")
	return New(&Deps{Tel: tel, Ctrl: ctrl, CtrlMu: &sync.Mutex{}, MPC: svc}), ctrl
}

func TestPlanNowExplainsTheGap(t *testing.T) {
	srv, _ := planNowSite(t, -1231)
	got := srv.planNow(time.Now(), nil)
	if got == nil || got.Status != planNowOffPlan || got.Why != "less_solar" {
		t.Fatalf("plan_now = %+v", got)
	}
	if got.PlannedBatteryW != 2595 || got.BatteryW != -1231 || got.PlannedPVW != -3814 || got.PVW == nil || *got.PVW != -1392 {
		t.Fatalf("numbers = %+v", got)
	}
	if got.SlotEndMs-got.SlotStartMs != 15*60_000 {
		t.Fatalf("quarter = %+v", got)
	}
}

func TestPlanNowAsPlannedHasNoWhy(t *testing.T) {
	srv, _ := planNowSite(t, 2500)
	got := srv.planNow(time.Now(), nil)
	if got == nil || got.Status != planNowAsPlanned || got.Why != "" {
		t.Fatalf("plan_now = %+v", got)
	}
}

func TestPlanNowNamesADeviceThatCannotFollow(t *testing.T) {
	srv, _ := planNowSite(t, -1231)
	got := srv.planNow(time.Now(), []ControlFeedback{
		{Kind: "ev", Status: "limited", Reason: "fuse_limit"},
		{Kind: "battery", Status: "limited", Reason: "battery_nearly_empty"},
	})
	if got == nil || got.Why != "device" || got.DeviceReason != "battery_nearly_empty" {
		t.Fatalf("plan_now = %+v", got)
	}
}

func TestPlanNowSaysTheOwnerHoldsTheBattery(t *testing.T) {
	srv, ctrl := planNowSite(t, -1231)
	ctrl.ManualHold = control.BatteryManualHold{PowerW: -1231, ExpiresAt: time.Now().Add(time.Hour)}
	got := srv.planNow(time.Now(), []ControlFeedback{{Kind: "battery", Status: "limited", Reason: "core_limit"}})
	if got == nil || got.Why != "manual" {
		t.Fatalf("plan_now = %+v", got)
	}
}

func TestPlanNowIsNilWithoutAPlanSteering(t *testing.T) {
	srv, ctrl := planNowSite(t, -1231)
	ctrl.Mode = control.ModeSelfConsumption
	if got := srv.planNow(time.Now(), nil); got != nil {
		t.Fatalf("another mode: %+v", got)
	}
	ctrl.Mode = control.ModePlannerArbitrage
	ctrl.PlanStale = true
	if got := srv.planNow(time.Now(), nil); got != nil {
		t.Fatalf("stale plan: %+v", got)
	}
	ctrl.PlanStale = false
	if got := srv.planNow(time.Now().Add(time.Hour), nil); got != nil {
		t.Fatalf("no quarter for now: %+v", got)
	}
}

func TestStatusSaysAsPlannedOnlyWhenItIs(t *testing.T) {
	srv, ctrl := planNowSite(t, -1231)
	ctrl.LastTargets = []control.DispatchTarget{{Driver: "sungrow", TargetW: -1231}}
	srv.deps.CapMu, srv.deps.CfgMu, srv.deps.Cfg = &sync.RWMutex{}, &sync.RWMutex{}, &config.Config{}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		PlanNow *struct {
			Status string `json:"status"`
			Why    string `json:"why"`
		} `json:"plan_now"`
		ControlFeedback []struct {
			Kind   string `json:"kind"`
			OnPlan *bool  `json:"on_plan"`
		} `json:"control_feedback"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PlanNow == nil || body.PlanNow.Status != planNowOffPlan || body.PlanNow.Why != "less_solar" {
		t.Fatalf("plan_now = %+v", body.PlanNow)
	}
	if len(body.ControlFeedback) == 0 {
		t.Fatal("no control rows")
	}
	for _, r := range body.ControlFeedback {
		if r.Kind == "battery" && (r.OnPlan == nil || *r.OnPlan) {
			t.Fatalf("battery row on_plan = %v", r.OnPlan)
		}
	}
}

func TestJournalKeepsThePlanAnswer(t *testing.T) {
	srv, _ := planNowSite(t, -1231)
	for _, v := range srv.JournalVerdicts(time.Now()) {
		if v.Subject != "site:plan" {
			continue
		}
		if v.Status != planNowOffPlan || v.Reason != "less_solar" || v.Severity != "info" ||
			v.RequestedW == nil || *v.RequestedW != 2595 || v.ActualW == nil || *v.ActualW != -1231 {
			t.Fatalf("verdict = %+v", v)
		}
		return
	}
	t.Fatal("no site:plan verdict")
}
