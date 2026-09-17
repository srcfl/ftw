package loadpoint

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestVehicleLimitPlanningDoesNotFinishAtEstimated80(t *testing.T) {
	st := State{TargetSoC: .8, CurrentSoC: .800106752726535, FinishAtVehicleLimit: true}
	if got := PlanningTarget(st, 0); got != 1 || got <= st.CurrentSoC {
		t.Fatal(got)
	}
	if got := PlanningTarget(st, .9); got != .9 {
		t.Fatal(got)
	}
	st.FinishAtVehicleLimit = false
	if got := PlanningTarget(st, .9); got != .8 {
		t.Fatal("changed explicit lower target", got)
	}
	if got := PlanningTarget(st, .7); got != .7 {
		t.Fatal("ignored car limit", got)
	}
}

func TestVehicleLimitCompletionKeepsChargingAndSafetyWins(t *testing.T) {
	now := time.Date(2026, 9, 17, 4, 0, 0, 0, time.UTC)
	cfg := chargeNowLoadpoint()
	cfg.PluginSoC = 1 // An exhausted estimate cannot prove the car is done.
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true, PowerW: 3600}}
	sender := &fakeSender{}
	dir := &Directive{SlotStart: now.Add(-time.Second), SlotEnd: now.Add(time.Hour), LoadpointEnergyWh: map[string]float64{cfg.ID: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.manager.SetSchedule(cfg.ID, Schedule{FinishAtVehicleLimit: true, TimeOfDayMinUTC: 5 * 60, Recurring: true})
	c.manager.RollSchedules(now)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	check := func(at time.Time, allowed bool, wantPositive bool) {
		t.Helper()
		c.TickWithDispatch(context.Background(), at, allowed)
		last, ok := lastSetCurrent(sender.calls)
		if !ok || (last.power > 0) != wantPositive {
			t.Fatal(last, ok)
		}
	}
	check(now, true, true)
	c.SetFuseEVMax(func() (float64, bool) { return 0, true })
	check(now.Add(time.Second), true, false)
	c.SetFuseEVMax(nil)
	// A full safety standdown does not erase the pending goal.
	check(now.Add(6*time.Minute), false, false)
	check(now.Add(6*time.Minute+time.Second), true, true)
	c.SetManualHold(cfg.ID, ManualHold{Persistent: true, PowerW: 0})
	check(now.Add(7*time.Minute), true, false)
	c.ClearManualHold(cfg.ID)
	check(now.Add(8*time.Minute), true, true)
	// Fresh BMS below its limit must continue after a passed deadline.
	c.SetVehicleChargeState(func(string) (VehicleChargeState, bool) {
		return VehicleChargeState{SoC: .78, Limit: .8, State: "Charging"}, true
	})
	c.manager.RollSchedules(now.Add(2 * time.Hour))
	check(now.Add(2*time.Hour), true, true)
	st, _ := c.manager.State(cfg.ID)
	if st.TargetTime.After(now.Add(time.Hour)) {
		t.Fatal("unfinished goal moved to tomorrow", st.TargetTime)
	}
	// Complete is the car's choice, including a lower car-side limit.
	c.SetVehicleChargeState(func(string) (VehicleChargeState, bool) {
		return VehicleChargeState{SoC: .8, Limit: .8, State: "Complete"}, true
	})
	samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: false}
	check(now.Add(2*time.Hour+time.Second), true, false)
	// Stale vehicle information must not hold a false Complete indefinitely.
	c.SetVehicleChargeState(func(string) (VehicleChargeState, bool) {
		return VehicleChargeState{SoC: .8, Limit: .8, State: "Complete"}, false
	})
	check(now.Add(2*time.Hour+2*time.Second), true, true)
}

func TestVehicleLimitKeepsPricePauseBeforeFinishing(t *testing.T) {
	now := time.Now().UTC()
	cfg := chargeNowLoadpoint()
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	sender := &fakeSender{}
	dir := &Directive{SlotStart: now.Add(-time.Second), SlotEnd: now.Add(time.Hour), LoadpointEnergyWh: map[string]float64{cfg.ID: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.manager.SetSchedule(cfg.ID, Schedule{FinishAtVehicleLimit: true, TimeOfDayMinUTC: (now.Hour()*60 + now.Minute() + 60) % 1440, Recurring: true})
	c.manager.RollSchedules(now)
	c.Tick(context.Background(), now)
	if got, ok := lastSetCurrent(sender.calls); !ok || got.power != 0 {
		t.Fatal(got)
	}
}

func TestVehicleLimitDeadlineSurvivesSameSessionRestart(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	before := time.Date(2026, 9, 17, 4, 0, 0, 0, time.UTC)
	schedule := Schedule{FinishAtVehicleLimit: true, TimeOfDayMinUTC: 5 * 60, Recurring: true}
	m := sessionManager(store, "garage", "charger")
	m.SetSchedule("garage", schedule)
	m.RollSchedules(before)
	schedule, _ = m.GetSchedule("garage")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:test", "session-a")
	deadline := before.Add(time.Hour)
	restart := func(session string) *Manager {
		next := sessionManager(store, "renamed", "renamed-driver")
		next.HydrateSchedules(func(string) (Schedule, bool) { return schedule, true })
		next.RollSchedules(before.Add(2 * time.Hour))
		next.ObserveSession("renamed", true, 4300, 1200, true, "easee:test", session)
		return next
	}
	same := restart("session-a")
	st, _ := same.State("renamed")
	if st.TargetTime != deadline || st.GoalRetention != "session" || st.SoCSource != "assumed" {
		t.Fatal(st)
	}
	next := restart("session-b")
	st, _ = next.State("renamed")
	if !st.TargetTime.After(before.Add(2 * time.Hour)) {
		t.Fatal("old deadline crossed a plug session", st.TargetTime)
	}
}

func TestVehicleLimitScheduleRoundTripAndExplicitTarget(t *testing.T) {
	var schedule Schedule
	if err := json.Unmarshal([]byte(`{"finish_at_vehicle_limit":true,"time_of_day_min_utc":300,"recurring":true}`), &schedule); err != nil {
		t.Fatal(err)
	}
	if schedule.Empty() || !schedule.HasTarget() {
		t.Fatal(schedule)
	}
	m := sessionManager(&sessionMemory{data: map[string]string{}}, "garage", "charger")
	m.SetSchedule("garage", schedule)
	m.RollSchedules(time.Now())
	m.SetTarget("garage", .6, time.Now().Add(time.Hour))
	if st, _ := m.State("garage"); st.FinishAtVehicleLimit || st.TargetSoC != .6 {
		t.Fatal(st)
	}
}

func TestVehicleLimitOneShotCompletionSurvivesNewSessionAndRestart(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	now := time.Date(2026, 9, 17, 4, 0, 0, 0, time.UTC)
	m := sessionManager(store, "garage", "charger")
	m.SetNowFn(func() time.Time { return now })
	m.SetSchedule("garage", Schedule{FinishAtVehicleLimit: true, TimeOfDayMinUTC: 5 * 60})
	m.RollSchedules(now)
	goal, _ := m.GetSchedule("garage")
	if goal.IntentID == "" || goal.FirstDeadlineMS != now.Add(time.Hour).UnixMilli() {
		t.Fatal(goal)
	}
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:test", "session-a")
	m.completeVehicleGoal("garage")
	m.ObserveSession("garage", false, 0, 1000, false, "easee:test", "")
	next := sessionManager(store, "garage", "charger")
	next.HydrateSchedules(func(string) (Schedule, bool) { return goal, true })
	next.RollSchedules(now.Add(2 * time.Hour))
	next.ObserveSession("garage", true, 0, 0, true, "easee:test", "session-b")
	st, _ := next.State("garage")
	if !st.GoalComplete || st.TargetSoC != 0 || st.GoalRetention != "session" {
		t.Fatal(st)
	}
	// Saving the same visible settings creates a new explicit request.
	next.SetSchedule("garage", goal)
	next.RollSchedules(now)
	next.ObserveSession("garage", true, 0, 0, true, "easee:test", "session-b")
	st, _ = next.State("garage")
	if st.GoalComplete || st.TargetSoC != 1 || st.Schedule.IntentID == goal.IntentID {
		t.Fatal(st)
	}
}
