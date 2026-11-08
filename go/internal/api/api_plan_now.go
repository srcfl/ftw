package api

import (
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/journal"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const (
	planNowAsPlanned = "as_planned"
	planNowOffPlan   = "off_plan"

	// Below this a battery counts as idle.
	planNowIdleW = 100.0
	// A gap this large and this share of the plan counts as off plan.
	planNowFloorW = 300.0
	planNowShare  = 0.3
	// A change must hold this long, so a passing cloud changes nothing.
	planNowHold = time.Minute
	// Readings further apart than this say nothing about what held between.
	planNowGap = 30 * time.Second
	// A flow names the cause when it explains this much of the gap.
	planNowCauseFloorW = 200.0
	planNowCauseShare  = 0.4
)

// PlanNow says whether the batteries do what the plan meant for this quarter
// and, when they do not, why. Core decides it once so every client says the
// same thing. Watts follow the site convention: charge and load positive,
// solar negative. Measured values are the last minute's mean.
//
// Why is empty as planned, else one of: manual (the owner holds the battery),
// device (a battery cannot follow; DeviceReason is its control_feedback
// reason), less_solar, more_solar, more_home, less_home, more_car, less_car,
// or other when no measured flow explains the gap.
type PlanNow struct {
	Status       string `json:"status"`
	Why          string `json:"why,omitempty"`
	DeviceReason string `json:"device_reason,omitempty"`
	SinceMs      int64  `json:"since_ms"`
	// SettlesAtMs is when a change under way takes effect, if it holds, so a
	// client that reads only on request knows when to ask again.
	SettlesAtMs     int64    `json:"settles_at_ms,omitempty"`
	SlotStartMs     int64    `json:"slot_start_ms"`
	SlotEndMs       int64    `json:"slot_end_ms"`
	BatteryW        float64  `json:"battery_w"`
	PlannedBatteryW float64  `json:"planned_battery_w"`
	PVW             *float64 `json:"pv_w"`
	PlannedPVW      float64  `json:"planned_pv_w"`
	LoadW           *float64 `json:"load_w"`
	PlannedLoadW    float64  `json:"planned_load_w"`
	EVW             float64  `json:"ev_w"`
	PlannedEVW      float64  `json:"planned_ev_w"`
}

// planOff reports whether a battery's power is off a plan: the other way, or
// by at least planNowFloorW and planNowShare of the plan.
func planOff(plannedW, actualW float64) bool {
	if (plannedW > planNowIdleW && actualW < -planNowIdleW) || (plannedW < -planNowIdleW && actualW > planNowIdleW) {
		return true
	}
	return math.Abs(plannedW-actualW) >= math.Max(planNowFloorW, planNowShare*math.Abs(plannedW))
}

type planNowSample struct {
	at                    time.Time
	battery, pv, load, ev float64
}

// planNowTracker settles the answer over time. The journal samples it every
// few seconds and /api/status reads it between those samples.
type planNowTracker struct {
	mu      sync.Mutex
	status  string
	since   time.Time
	pending time.Time
	samples []planNowSample
}

func (t *planNowTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status, t.since, t.pending, t.samples = "", time.Time{}, time.Time{}, nil
}

// observe takes one reading and answers with the settled status, since when
// it holds, when a change under way settles (zero if none), and the last
// minute's means. A change holds for planNowHold and is dated from when it
// began. The first reading settles at once.
func (t *planNowTracker) observe(s planNowSample, off bool) (string, time.Time, time.Time, planNowSample) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.samples); n > 0 {
		gap := s.at.Sub(t.samples[n-1].at)
		if gap < time.Second {
			return t.status, t.since, t.settles(), t.mean()
		}
		if gap > planNowGap {
			t.status, t.pending, t.samples = "", time.Time{}, nil
		}
	}
	want := planNowAsPlanned
	if off {
		want = planNowOffPlan
	}
	switch {
	case t.status == "":
		t.status, t.since, t.pending = want, s.at, time.Time{}
	case t.status == want:
		t.pending = time.Time{}
	case t.pending.IsZero():
		t.pending = s.at
	case s.at.Sub(t.pending) >= planNowHold:
		t.status, t.since, t.pending = want, t.pending, time.Time{}
	}
	t.samples = append(t.samples, s)
	cut := s.at.Add(-planNowHold)
	first := 0
	for first < len(t.samples)-1 && t.samples[first].at.Before(cut) {
		first++
	}
	t.samples = append(t.samples[:0], t.samples[first:]...)
	return t.status, t.since, t.settles(), t.mean()
}

func (t *planNowTracker) settles() time.Time {
	if t.pending.IsZero() {
		return time.Time{}
	}
	return t.pending.Add(planNowHold)
}

func (t *planNowTracker) mean() planNowSample {
	var m planNowSample
	for _, s := range t.samples {
		m.battery += s.battery
		m.pv += s.pv
		m.load += s.load
		m.ev += s.ev
	}
	n := float64(len(t.samples))
	m.battery, m.pv, m.load, m.ev = m.battery/n, m.pv/n, m.load/n, m.ev/n
	return m
}

// planNowCause names the measured flow that moved the battery most in the
// direction of the gap. A battery that gets less than planned is short of
// what solar, the home or the car left it.
func planNowCause(p *PlanNow, m planNowSample) string {
	gap := m.battery - p.PlannedBatteryW
	type flow struct {
		name  string
		moved float64
	}
	var flows []flow
	add := func(lower, higher string, moved float64) {
		name := higher
		if moved < 0 {
			name = lower
		}
		flows = append(flows, flow{name, moved})
	}
	if p.PVW != nil {
		add("less_solar", "more_solar", p.PlannedPVW-m.pv)
	}
	if p.LoadW != nil {
		add("more_home", "less_home", p.PlannedLoadW-m.load)
	}
	add("more_car", "less_car", p.PlannedEVW-m.ev)
	best, bestW := "", 0.0
	for _, f := range flows {
		if w := math.Copysign(1, gap) * f.moved; w > bestW {
			best, bestW = f.name, w
		}
	}
	if bestW < math.Max(planNowCauseFloorW, planNowCauseShare*math.Abs(gap)) {
		return "other"
	}
	return best
}

func plannedEVW(a *mpc.Action) float64 {
	if len(a.LoadpointPowerW) == 0 {
		return a.LoadpointW
	}
	total := 0.0
	for _, w := range a.LoadpointPowerW {
		total += w
	}
	return total
}

// planNow compares the batteries with the plan's current quarter. It is nil
// when no plan steers them: another mode, a stale plan, no quarter for now,
// or no battery reading.
func (s *Server) planNow(now time.Time, rows []ControlFeedback) *PlanNow {
	if s.deps.Tel == nil || s.deps.Ctrl == nil || s.deps.CtrlMu == nil || s.deps.MPC == nil {
		return nil
	}
	s.deps.CtrlMu.Lock()
	mode, stale, meter := s.deps.Ctrl.Mode, s.deps.Ctrl.PlanStale, s.deps.Ctrl.SiteMeterDriver
	_, held := s.deps.Ctrl.GetBatteryManualHold(now)
	s.deps.CtrlMu.Unlock()
	slot := activeAction(s.deps.MPC.Latest(), now)
	if !mode.IsPlannerMode() || stale || slot == nil {
		s.planNowState.reset()
		return nil
	}
	var opts telemetry.ForecastOptions
	if s.deps.HistoryMeasurementSources != nil {
		opts = s.deps.HistoryMeasurementSources()
	}
	live := s.deps.Tel.LiveMeasurementNow(meter, opts)
	if !live.Known[telemetry.DerBattery] {
		s.planNowState.reset()
		return nil
	}
	sample := planNowSample{at: now, battery: live.BatteryW, pv: live.PVW, load: live.LoadW, ev: live.EVW}
	status, since, settles, mean := s.planNowState.observe(sample, planOff(slot.BatteryW, live.BatteryW))
	out := &PlanNow{
		Status: status, SinceMs: since.UnixMilli(),
		SlotStartMs: slot.SlotStartMs, SlotEndMs: slot.SlotStartMs + int64(slot.SlotLenMin)*60_000,
		BatteryW: mean.battery, PlannedBatteryW: slot.BatteryW,
		PlannedPVW: slot.PVW, PlannedLoadW: slot.LoadW,
		EVW: mean.ev, PlannedEVW: plannedEVW(slot),
	}
	if !settles.IsZero() {
		out.SettlesAtMs = settles.UnixMilli()
	}
	if live.Known[telemetry.DerPV] {
		out.PVW = &mean.pv
	}
	if live.LoadKnown {
		out.LoadW = &mean.load
	}
	if status != planNowOffPlan {
		return out
	}
	reason := ""
	for _, r := range rows {
		if r.Kind == "battery" && (r.Status == "limited" || r.Status == "not_following" || r.Status == "no_contact" || r.Status == "not_controlled") {
			reason = r.Reason
			break
		}
	}
	switch {
	case held:
		out.Why = "manual"
	case reason != "":
		out.Why, out.DeviceReason = "device", reason
	default:
		out.Why = planNowCause(out, mean)
	}
	return out
}

// markOnPlan tells each battery row whether "as planned" is true.
func markOnPlan(rows []ControlFeedback, plan *PlanNow) {
	if plan == nil {
		return
	}
	on := plan.Status == planNowAsPlanned
	for i := range rows {
		if rows[i].Kind == "battery" {
			rows[i].OnPlan = &on
		}
	}
}

// planNowVerdict is the journal's record of the same answer.
func planNowVerdict(plan *PlanNow) journal.Verdict {
	detail, _ := json.Marshal(plan)
	return journal.Verdict{
		Subject: "site:plan", Kind: "plan", Status: plan.Status, Reason: plan.Why, Severity: "info",
		RequestedW: watts(plan.PlannedBatteryW), ActualW: watts(plan.BatteryW), Detail: detail,
	}
}
