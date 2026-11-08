package api

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// evTrace is an Easee Cloud charger on a loadpoint: a command and a poll
// every five seconds, as Core and the driver send them. The cloud stamps a
// power value when it changes and each poll confirms it (easee_cloud.lua).
type evTrace struct {
	tel       *telemetry.Store
	mgr       *loadpoint.Manager
	srv       *Server
	changedAt time.Time
	drawW     float64
	extra     string // more reading fields, each with a leading comma
}

func newEVTrace(drawW float64) *evTrace {
	tel := telemetry.NewStore()
	tel.DriverHealthMut("easee")
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040}})
	return &evTrace{tel: tel, mgr: mgr, srv: New(&Deps{Tel: tel, Loadpoints: mgr}), changedAt: time.Now(), drawW: drawW}
}

// tick orders w for reason, polls the charger and waits five seconds.
func (e *evTrace) tick(w float64, reason string) {
	c := e.tel.BeginCommand("easee", []byte(fmt.Sprintf(`{"action":"ev_set_current","power_w":%g}`, w)), time.Now())
	e.tel.CompleteCommand(c, "accepted")
	e.mgr.SetCommanded("garage", w, reason)
	e.tel.Update("easee", telemetry.DerEV, e.drawW, nil, []byte(fmt.Sprintf(
		`{"connected":true,"control_power_observed_at":%q,"control_power_confirmed":true,"power_max_age_s":180%s}`,
		e.changedAt.Format(time.RFC3339Nano), e.extra)))
	e.mgr.Observe("garage", true, e.drawW, 0, true)
	time.Sleep(5 * time.Second)
}

func (e *evTrace) verdict(t *testing.T) ControlFeedback {
	t.Helper()
	for _, f := range e.srv.controlFeedback(time.Now()) {
		if f.Kind == "ev" {
			return f
		}
	}
	t.Fatal("no EV verdict")
	return ControlFeedback{}
}

// Field report, 22 Oct 2026, v0.155.0-beta.1, Easee Cloud: the charger drew
// 3.13 kW, Core cut its order to 0 W for the main fuse at 06:32 and the
// charger kept drawing. The verdict said "Limited by the main fuse" for as
// long as it drew. A limit explains a lower order, never a draw above it.
func TestChargerDrawingAgainstAFuseStopIsNotFollowing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEVTrace(3130)
		for range 60 * 60 / 5 {
			e.tick(7590, "plan")
		}
		cut := time.Now()
		var first, alarm time.Duration
		for range 20 * 60 / 5 {
			e.tick(0, "fuse_limit")
			f, since := e.verdict(t), time.Since(cut)
			if f.Status == "not_following" && first == 0 {
				first = since
				if f.Reason != "power_while_idle" || f.Severity != "warning" || f.Evidence != "measured" {
					t.Fatalf("first not following at %v: %s/%s/%s", since, f.Reason, f.Severity, f.Evidence)
				}
			}
			if f.Severity == "alarm" && alarm == 0 {
				alarm = since
				if f.Status != "not_following" {
					t.Fatalf("alarm at %v is %s/%s", since, f.Status, f.Reason)
				}
			}
			if first != 0 && f.Status != "not_following" {
				t.Fatalf("at %v the verdict went back to %s/%s while the charger still draws", since, f.Status, f.Reason)
			}
		}
		t.Logf("not following from %v after the cut, alarm from %v", first, alarm)
		// Easee declares 180 s for its power to show; Core waits two minutes
		// for an order to take hold and two more of measured disagreement.
		if first < 3*time.Minute || first > 5*time.Minute {
			t.Fatalf("not following after %v of drawing against a stop, want within 3-5 min", first)
		}
		if alarm < first+10*time.Minute || alarm > first+11*time.Minute {
			t.Fatalf("alarm after %v (not following from %v), want ten minutes later", alarm, first)
		}
	})
}

// A charger that obeys the cut within its response time stays Limited,
// including Easee Cloud at the end of its declared 180 s.
func TestChargerFollowingAFuseStopStaysLimited(t *testing.T) {
	for _, after := range []time.Duration{100 * time.Second, 175 * time.Second} {
		synctest.Test(t, func(t *testing.T) {
			e := newEVTrace(7500)
			for range 10 * 60 / 5 {
				e.tick(7590, "plan")
			}
			cut := time.Now()
			for range 15 * 60 / 5 {
				if time.Since(cut) >= after && e.drawW > 0 {
					e.drawW, e.changedAt = 0, time.Now()
				}
				e.tick(0, "fuse_limit")
				if f := e.verdict(t); f.Status != "limited" || f.Reason != "fuse_limit" || f.Severity == "alarm" {
					t.Fatalf("stopped after %v: at %v the verdict is %s/%s/%s", after, time.Since(cut), f.Status, f.Reason, f.Severity)
				}
			}
		})
	}
}

// Time spent drawing below the order is not time drawing above it. After
// 13 minutes at 3 kW against a 7 kW order, a car that rises to 9 kW is
// drawing more than it is told: a warning now, an alarm only ten minutes
// after that wait has passed for the overdraw itself.
func TestOverdrawAfterAShortfallAlarmsOnItsOwnTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEVTrace(3000)
		for range 13 * 60 / 5 {
			e.tick(7000, "plan")
		}
		if f := e.verdict(t); f.Reason != "power_below_target" || f.Severity != "warning" {
			t.Fatalf("3 kW against 7 kW reads %s/%s", f.Reason, f.Severity)
		}
		e.drawW, e.changedAt = 9000, time.Now()
		over := time.Now()
		var alarm time.Duration
		for range 15 * 60 / 5 {
			e.tick(7000, "plan")
			f := e.verdict(t)
			if f.Reason != "power_above_target" {
				t.Fatalf("at %v 9 kW against 7 kW reads %s", time.Since(over), f.Reason)
			}
			if f.Severity == "alarm" && alarm == 0 {
				alarm = time.Since(over)
			}
		}
		if alarm < 12*time.Minute || alarm > 13*time.Minute {
			t.Fatalf("overdraw alarmed after %v, want two minutes plus ten", alarm)
		}
	})
}

// A car that takes less than a fuse-limited order is not the fuse's doing.
// It reads as it would under a plan order: not following, a warning.
func TestCarBelowAFuseLimitedOrderIsNotFollowing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEVTrace(3130)
		for range 10 * 60 / 5 {
			e.tick(8280, "fuse_limit")
		}
		f := e.verdict(t)
		if f.Status != "not_following" || f.Reason != "power_below_target" || f.Severity != "warning" {
			t.Fatalf("3.1 kW against an 8.3 kW fuse-limited order reads %s/%s/%s", f.Status, f.Reason, f.Severity)
		}
		for range 30 * 60 / 5 {
			e.tick(8280, "fuse_limit")
		}
		if f := e.verdict(t); f.Severity != "warning" {
			t.Fatalf("a car drawing less than offered escalated to %s", f.Severity)
		}
	})
}
