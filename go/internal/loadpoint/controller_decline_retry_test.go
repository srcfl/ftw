package loadpoint

import (
	"context"
	"testing"
	"time"
)

// declineRetryRig drives one charger through the field case of
// srcfl/ftw#2080. The plan offers the car current only while the loadpoint
// has not latched a refusal, as Core's planner does. The car declines every
// offer when refuses is set. Under Core's own 0 A the charger reports the car
// as asking, as an Easee does with reason 52.
type declineRetryRig struct {
	t       *testing.T
	c       *Controller
	cfg     Config
	dir     *Directive
	samples map[string]EVSample
	clock   time.Time
	refuses bool
	offers  []time.Time // when an offer began after Core held 0 A
}

func newDeclineRetryRig(t *testing.T) *declineRetryRig {
	t.Helper()
	r := &declineRetryRig{
		t:     t,
		cfg:   reasonLoadpoint(),
		dir:   &Directive{},
		clock: time.Date(2026, 11, 24, 21, 45, 0, 0, time.UTC),
	}
	r.samples = map[string]EVSample{r.cfg.DriverName: {Connected: true, PowerW: 11000, RequestActive: true}}
	r.c = newTestController(t, []Config{r.cfg}, r.dir, r.samples, &fakeSender{})
	r.c.manager.SetNowFn(func() time.Time { return r.clock })
	r.c.manager.SetLocation(time.UTC)
	r.c.manager.SetTarget(r.cfg.ID, 1, r.clock.Add(9*time.Hour))
	return r
}

func (r *declineRetryRig) state() State {
	r.t.Helper()
	st, ok := r.c.manager.State(r.cfg.ID)
	if !ok {
		r.t.Fatal("loadpoint missing")
	}
	return st
}

// step advances the clock and runs one control tick the way main does:
// roll schedules, plan, then the controller.
func (r *declineRetryRig) step(d time.Duration) {
	r.t.Helper()
	before := r.state()
	r.clock = r.clock.Add(d)
	offered := before.CommandedKnown && before.CommandedW >= DeliveringW
	switch {
	case !r.refuses:
		r.samples[r.cfg.DriverName] = EVSample{Connected: true, PowerW: before.CommandedW, RequestActive: true}
	case offered:
		r.samples[r.cfg.DriverName] = EVSample{Connected: true, PowerW: 0, RequestActive: false}
	default:
		r.samples[r.cfg.DriverName] = EVSample{Connected: true, PowerW: 0, RequestActive: true}
	}
	r.c.manager.RollSchedules(r.clock)
	slot := r.clock.Truncate(15 * time.Minute)
	r.dir.SlotStart, r.dir.SlotEnd = slot, slot.Add(15*time.Minute)
	r.dir.LoadpointEnergyWh = map[string]float64{}
	if r.samples[r.cfg.DriverName].Connected && !r.state().ChargingDeclined {
		r.dir.LoadpointEnergyWh[r.cfg.ID] = 2750
	}
	r.c.Tick(context.Background(), r.clock)
	if after := r.state(); !offered && after.CommandedW >= DeliveringW && before.CommandedKnown {
		r.offers = append(r.offers, r.clock)
	}
}

func (r *declineRetryRig) run(d, every time.Duration) {
	r.t.Helper()
	for end := r.clock.Add(d); r.clock.Before(end); {
		r.step(every)
	}
}

// decline makes the car refuse a live offer until the latch forms and the
// next plan leaves the car out, and returns when the latch formed.
func (r *declineRetryRig) decline() time.Time {
	r.t.Helper()
	r.step(5 * time.Second)
	if st := r.state(); st.CommandedW < DeliveringW || st.CommandedReason != "plan" {
		r.t.Fatalf("plan did not offer current: %+v", st)
	}
	r.refuses = true
	for start := r.clock; r.clock.Sub(start) < 3*SessionCompletionTimeout; {
		r.step(5 * time.Second)
		if r.state().ChargingDeclined {
			latched := r.clock
			r.step(5 * time.Second)
			r.wantLatched("once the plan leaves the car out")
			return latched
		}
	}
	r.t.Fatal("a lasting refusal under a live offer did not latch")
	return time.Time{}
}

func (r *declineRetryRig) wantLatched(why string) {
	r.t.Helper()
	if st := r.state(); !st.ChargingDeclined || st.CommandedW != 0 || st.CommandedReason != "vehicle_not_requesting" {
		r.t.Fatalf("%s: want the refusal latched at 0 W (vehicle_not_requesting), got declined=%v %.0f W %q",
			why, st.ChargingDeclined, st.CommandedW, st.CommandedReason)
	}
}

func (r *declineRetryRig) wantOffered(why string) {
	r.t.Helper()
	if st := r.state(); st.ChargingDeclined || st.CommandedW < DeliveringW || st.CommandedReason != "plan" {
		r.t.Fatalf("%s: want the car planned and offered current, got declined=%v %.0f W %q",
			why, st.ChargingDeclined, st.CommandedW, st.CommandedReason)
	}
}

// A car that declined is planned and offered current again once
// DeclineRetryInterval has passed, while it stays plugged in.
func TestDeclinedCarIsOfferedAgainAfterTheRetryInterval(t *testing.T) {
	r := newDeclineRetryRig(t)
	latched := r.decline()
	r.run(DeclineRetryInterval-time.Minute-r.clock.Sub(latched), 30*time.Second)
	r.wantLatched("before the retry interval")

	r.refuses = false
	r.run(2*time.Minute, 30*time.Second)
	r.wantOffered("after the retry interval")
	if len(r.offers) != 1 {
		t.Fatalf("want one new offer, got %v", r.offers)
	}
	if got := r.offers[0].Sub(latched); got < DeclineRetryInterval {
		t.Fatalf("offered again %v after the refusal, before the %v interval", got, DeclineRetryInterval)
	}
}

// A new goal window, the next recurring deadline, also brings one more
// offer, even when the car declined less than an interval before.
func TestDeclinedCarIsOfferedAgainAtTheNextRecurringDeadline(t *testing.T) {
	r := newDeclineRetryRig(t)
	r.clock = time.Date(2026, 11, 25, 6, 30, 0, 0, time.UTC)
	r.c.manager.SetSchedule(r.cfg.ID, Schedule{SoC: 1, TimeOfDayMinUTC: 7 * 60, Recurring: true})
	r.c.manager.RollSchedules(r.clock)
	deadline := r.state().TargetTime
	if !deadline.Equal(time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("recurring deadline: %v", deadline)
	}
	r.decline()
	r.run(deadline.Sub(r.clock)-time.Minute, 30*time.Second)
	r.wantLatched("before the deadline")

	r.refuses = false
	r.run(2*time.Minute, 30*time.Second)
	if next := r.state().TargetTime; !next.Equal(deadline.Add(24 * time.Hour)) {
		t.Fatalf("deadline did not roll: %v", next)
	}
	r.wantOffered("at the next goal window")
}

// A car that always declines is offered current at most once per
// DeclineRetryInterval, deadline rolls included. Each offer ends with the
// usual SessionCompletionTimeout, and the plan drops the car again.
func TestCarThatKeepsDecliningIsNotFlapped(t *testing.T) {
	r := newDeclineRetryRig(t)
	r.clock = time.Date(2026, 11, 25, 3, 15, 0, 0, time.UTC)
	r.c.manager.SetSchedule(r.cfg.ID, Schedule{SoC: 1, TimeOfDayMinUTC: 7 * 60, Recurring: true})
	r.c.manager.RollSchedules(r.clock)
	r.decline()
	r.offers = nil
	r.run(5*time.Hour, 15*time.Second)

	if len(r.offers) < 4 || len(r.offers) > 5 {
		t.Fatalf("five hours of refusals: want an offer about once an hour, got %d: %v", len(r.offers), r.offers)
	}
	for i := 1; i < len(r.offers); i++ {
		if gap := r.offers[i].Sub(r.offers[i-1]); gap < DeclineRetryInterval {
			t.Fatalf("offers %v apart, under the %v interval: %v", gap, DeclineRetryInterval, r.offers)
		}
	}
	r.wantLatched("between retries")
}

// No retry once the goal is met, the car's own fresh reading says it is
// done, a one-shot deadline has passed, or Core may not command the charger.
func TestDeclinedCarRetryNeedsAGoalStillToMeet(t *testing.T) {
	none := func(*declineRetryRig) {}
	for _, tc := range []struct {
		name          string
		before, after func(r *declineRetryRig)
		retry         bool
	}{
		{"goal not met", none, none, true},
		{"goal lowered below the level", none, func(r *declineRetryRig) {
			r.c.manager.SetTarget(r.cfg.ID, 0.1, r.state().TargetTime)
		}, false},
		{"car at its own limit", none, func(r *declineRetryRig) {
			r.c.SetVehicleChargeState(func(string) (VehicleChargeState, bool) {
				return VehicleChargeState{SoC: .8, Limit: .8, State: "Stopped"}, true
			})
		}, false},
		{"car reports complete", none, func(r *declineRetryRig) {
			r.c.SetVehicleChargeState(func(string) (VehicleChargeState, bool) {
				return VehicleChargeState{SoC: .6, State: "Complete"}, true
			})
		}, false},
		{"one-shot deadline passed", func(r *declineRetryRig) {
			r.c.manager.SetTarget(r.cfg.ID, 1, r.clock.Add(20*time.Minute))
		}, none, false},
		{"charger Core may not command", none, func(r *declineRetryRig) {
			r.c.SetChargerControlAllowed(func(string) bool { return false })
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeclineRetryRig(t)
			tc.before(r)
			r.decline()
			tc.after(r)
			r.refuses = false
			r.run(DeclineRetryInterval+2*time.Minute, time.Minute)
			if got := !r.state().ChargingDeclined; got != tc.retry {
				t.Fatalf("retried=%v want %v: %+v", got, tc.retry, r.state())
			}
		})
	}
}

// The existing exits still end the refusal at once.
func TestDeclineExitsStillActAtOnce(t *testing.T) {
	t.Run("charge now", func(t *testing.T) {
		r := newDeclineRetryRig(t)
		r.decline()
		r.step(time.Minute)
		r.c.SetManualHold(r.cfg.ID, ManualHold{PowerW: 11040, PhaseMode: "3p", Persistent: true})
		if r.state().ChargingDeclined {
			t.Fatal("Charge now kept the refusal")
		}
	})
	t.Run("unplug", func(t *testing.T) {
		r := newDeclineRetryRig(t)
		r.decline()
		r.step(time.Minute)
		r.samples[r.cfg.DriverName] = EVSample{Connected: false}
		r.c.manager.ObserveSample(r.cfg.ID, r.samples[r.cfg.DriverName])
		if r.state().ChargingDeclined {
			t.Fatal("unplug kept the refusal")
		}
	})
}
