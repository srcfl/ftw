package api

// Site traces replayed through telemetry, command evidence and the API on a
// fake clock. They cover the verdict over time, not one classified instant.

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A hybrid inverter that relays its own grid meter, as on the Sungrow SH.
type hybridTrace struct {
	tel                           *telemetry.Store
	srv                           *Server
	houseW, pvW, batW, cmdW, socV float64
	verdicts                      []ControlFeedback
	at                            []time.Duration
	start                         time.Time
}

func newHybridTrace() *hybridTrace {
	tel := telemetry.NewStore()
	tel.DriverHealthMut("hybrid")
	ctrl := &control.State{Mode: control.ModeSelfConsumption, SiteMeterDriver: "hybrid"}
	srv := New(&Deps{Tel: tel, Ctrl: ctrl, CtrlMu: &sync.Mutex{}, Cfg: &config.Config{}, CfgMu: &sync.RWMutex{}})
	return &hybridTrace{tel: tel, srv: srv, socV: 0.5, start: time.Now()}
}

func (h *hybridTrace) poll() {
	h.tel.Update("hybrid", telemetry.DerMeter, h.houseW+h.pvW+h.batW, nil, []byte(`{"power_origin":"external_meter"}`))
	h.tel.Update("hybrid", telemetry.DerPV, h.pvW, nil, []byte(`{}`))
	soc := h.socV
	h.tel.Update("hybrid", telemetry.DerBattery, h.batW, &soc, []byte(fmt.Sprintf(`{"setpoint_w":%g}`, h.cmdW)))
}

func (h *hybridTrace) command(w float64) {
	h.cmdW = w
	c := h.tel.BeginCommand("hybrid", []byte(fmt.Sprintf(`{"action":"battery","power_w":%g}`, w)), time.Now())
	h.tel.CompleteCommand(c, "accepted")
}

func (h *hybridTrace) sample() {
	for _, f := range h.srv.controlFeedback(time.Now()) {
		if f.Kind == "battery" {
			h.verdicts = append(h.verdicts, f)
			h.at = append(h.at, time.Since(h.start))
		}
	}
}

// share is the fraction of samples taken after `from` that match.
func (h *hybridTrace) share(from time.Duration, match func(ControlFeedback) bool) float64 {
	n, hit := 0, 0
	for i, f := range h.verdicts {
		if h.at[i] < from {
			continue
		}
		n++
		if match(f) {
			hit++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(hit) / float64(n)
}

func (h *hybridTrace) changes() int {
	n := 0
	for i := 1; i < len(h.verdicts); i++ {
		if h.verdicts[i].Status != h.verdicts[i-1].Status {
			n++
		}
	}
	return n
}

// Self-consumption retunes the target every tick. A battery that follows each
// command must read as following, not as a stream of new response waits.
func TestTrackingTargetStaysFollowing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rng := rand.New(rand.NewSource(7))
		h := newHybridTrace()
		h.houseW, h.pvW = 700, -2500
		for range 15 * 60 / 5 {
			h.houseW = math.Max(150, h.houseW+rng.NormFloat64()*120)
			if rng.Float64() < 0.03 {
				h.houseW += 1500 * (rng.Float64() - 0.3)
			}
			h.command(math.Max(-5000, math.Min(5000, -(h.houseW+h.pvW))))
			time.Sleep(2500 * time.Millisecond)
			h.batW = h.cmdW + rng.NormFloat64()*15
			h.poll()
			h.sample()
			time.Sleep(2500 * time.Millisecond)
			h.poll()
			h.sample()
		}
		following := h.share(time.Minute, func(f ControlFeedback) bool { return f.Status == "following" })
		if following < 0.9 || h.changes() > 12 {
			t.Fatalf("following %.0f%% with %d status changes", following*100, h.changes())
		}
	})
}

// The same retuning must not hide a battery that stops following.
func TestTrackingTargetWarnsWhenIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rng := rand.New(rand.NewSource(11))
		h := newHybridTrace()
		h.houseW, h.pvW = 700, -2500
		for range 15 * 60 / 5 {
			h.houseW = math.Max(150, h.houseW+rng.NormFloat64()*120)
			h.command(math.Max(-5000, math.Min(5000, -(h.houseW+h.pvW))))
			time.Sleep(2500 * time.Millisecond)
			h.batW = h.cmdW + rng.NormFloat64()*15
			if time.Since(h.start) >= 5*time.Minute {
				h.batW = 0
			}
			h.poll()
			h.sample()
			time.Sleep(2500 * time.Millisecond)
			h.poll()
			h.sample()
		}
		warned := h.share(6*time.Minute, func(f ControlFeedback) bool {
			return f.Status == "not_following" && f.Severity == "warning"
		})
		if warned < 0.95 {
			t.Fatalf("ignored commands warned in %.0f%% of samples", warned*100)
		}
	})
}

// A kettle after a confirmed step changes the house, not the battery.
func TestConfirmedStepSurvivesLaterHouseholdLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHybridTrace()
		h.houseW, h.pvW = 600, -4800
		for range 30 {
			time.Sleep(time.Second)
			h.poll()
		}
		h.command(-2250)
		for sec := range 15 * 60 {
			time.Sleep(time.Second)
			if sec >= 2 {
				h.batW = -2250
			}
			switch sec {
			case 180:
				h.houseW += 2000
			case 360:
				h.houseW -= 2000
			}
			h.pvW = -4800 + 300*math.Sin(float64(sec)/90)
			if sec%5 == 0 {
				h.poll()
			}
			if sec%30 == 0 {
				h.command(-2250)
			}
			h.sample()
		}
		confirmed := h.share(2*time.Minute, func(f ControlFeedback) bool {
			return f.Status == "following" && f.Evidence == "confirmed" && f.ConfirmedAtMs > 0
		})
		if confirmed < 0.99 {
			t.Fatalf("confirmed in %.0f%% of samples after the step", confirmed*100)
		}
	})
}

// A battery takes less charge as it fills. Its fresh SoC explains that.
func TestChargeTaperNearFullIsInformation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHybridTrace()
		h.houseW, h.pvW, h.socV = 500, -4500, 0.95
		for range 30 {
			time.Sleep(time.Second)
			h.poll()
		}
		h.command(3000)
		for sec := range 20 * 60 {
			time.Sleep(time.Second)
			h.socV = math.Min(0.995, 0.95+float64(sec)/20/60*0.045)
			h.batW = 3000
			if h.socV > 0.96 {
				h.batW = math.Max(300, 3000*(1-(h.socV-0.96)/0.035))
			}
			if sec%5 == 0 {
				h.poll()
			}
			if sec%30 == 0 {
				h.command(3000)
			}
			h.sample()
		}
		if warned := h.share(0, func(f ControlFeedback) bool { return f.Severity != "info" }); warned > 0 {
			t.Fatalf("normal taper warned in %.0f%% of samples", warned*100)
		}
		if full := h.share(10*time.Minute, func(f ControlFeedback) bool { return f.Reason == "battery_nearly_full" }); full < 0.9 {
			t.Fatalf("taper explained in %.0f%% of samples", full*100)
		}
	})
}

// A cloud charger reports less often than a local inverter. Its declared
// power_max_age_s, not a local ten-second rule, decides freshness.
func TestCloudChargerReachesMeasuredFollowing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := telemetry.NewStore()
		tel.DriverHealthMut("cloud")
		srv := New(&Deps{Tel: tel})
		start := time.Now()
		var observedAt time.Time
		var verdicts []ControlFeedback
		var at []time.Duration
		for sec := range 20 * 60 {
			if sec%60 == 0 {
				c := tel.BeginCommand("cloud", []byte(`{"action":"ev_set_current","power_w":11000}`), time.Now())
				tel.CompleteCommand(c, "accepted")
			}
			time.Sleep(time.Second)
			if sec%60 == 30 {
				observedAt = time.Now().Add(-5 * time.Second)
			}
			if sec%10 == 0 && !observedAt.IsZero() {
				data := fmt.Sprintf(`{"connected":true,"control_power_observed_at":%q,"power_max_age_s":180}`, observedAt.Format(time.RFC3339Nano))
				tel.Update("cloud", telemetry.DerEV, 10900, nil, []byte(data))
			}
			for _, f := range srv.controlFeedback(time.Now()) {
				verdicts = append(verdicts, f)
				at = append(at, time.Since(start))
			}
		}
		n, following := 0, 0
		for i, f := range verdicts {
			if at[i] < 3*time.Minute {
				continue
			}
			n++
			if f.Status == "following" && f.Evidence == "measured" {
				following++
			}
		}
		if n == 0 || float64(following)/float64(n) < 0.95 {
			t.Fatalf("cloud charger following in %d of %d samples", following, n)
		}
	})
}

func TestControlStatusAnswersEveryReason(t *testing.T) {
	measured := 1
	for reason, want := range map[string][2]string{
		"power_observed":       {"following", "info"},
		"idle":                 {"following", "info"},
		"vehicle_complete":     {"following", "info"},
		"waiting_response":     {"waiting", "info"},
		"not_connected":        {"waiting", "info"},
		"no_plan_budget":       {"waiting", "info"},
		"battery_full":         {"limited", "info"},
		"battery_nearly_full":  {"limited", "info"},
		"fuse_limit":           {"limited", "info"},
		"load_balancer_limit":  {"limited", "info"},
		"device_limit":         {"limited", "warning"},
		"power_below_target":   {"not_following", "warning"},
		"setpoint_changed":     {"not_following", "warning"},
		"device_fault":         {"not_following", "alarm"},
		"telemetry_stale":      {"no_contact", "warning"},
		"command_failed":       {"no_contact", "warning"},
		"readings_lost":        {"no_contact", "alarm"},
		"default_failed":       {"no_contact", "alarm"},
		"observe_only":         {"not_controlled", "info"},
		"device_control":       {"not_controlled", "info"},
		"site_meter_stale":     {"not_controlled", "warning"},
		"unknown_future_cause": {"waiting", "info"},
	} {
		f := ControlFeedback{Reason: reason, VerificationTier: &measured}
		setControlStatus(&f)
		if f.Status != want[0] || f.Severity != want[1] || f.Evidence != "measured" {
			t.Errorf("%s: got %s/%s/%s, want %s/%s", reason, f.Status, f.Severity, f.Evidence, want[0], want[1])
		}
	}
	accepted := 0
	f := ControlFeedback{Reason: "power_observed", VerificationTier: &accepted}
	if setControlStatus(&f); f.Status != "waiting" || f.Evidence != "accepted" {
		t.Fatalf("a driver reply alone read as following: %+v", f)
	}
}

// Easee's cloud records power only when it changes. While the cloud still
// hears from the charger, each poll confirms the unchanged value, so a steady
// charge stays measured instead of turning into a lost-control alarm.
func TestChangeOnlyChargerStaysCurrentWhileSteady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := telemetry.NewStore()
		tel.DriverHealthMut("cloud")
		srv := New(&Deps{Tel: tel})
		start := time.Now()
		changedAt := time.Time{}
		power := 0.0
		var late []ControlFeedback
		for sec := range 15 * 60 {
			if sec%60 == 0 {
				c := tel.BeginCommand("cloud", []byte(`{"action":"ev_set_current","power_w":11000}`), time.Now())
				tel.CompleteCommand(c, "accepted")
			}
			time.Sleep(time.Second)
			// The charger ramps once, 40 s after the first command, then holds.
			if sec == 40 {
				power, changedAt = 10900, time.Now().Add(-3*time.Second)
			}
			if sec%10 == 0 && !changedAt.IsZero() {
				data := fmt.Sprintf(`{"connected":true,"control_power_observed_at":%q,"control_power_confirmed":true,"power_max_age_s":180}`, changedAt.Format(time.RFC3339Nano))
				tel.Update("cloud", telemetry.DerEV, power, nil, []byte(data))
			}
			if time.Since(start) >= 3*time.Minute {
				late = append(late, srv.controlFeedback(time.Now())...)
			}
		}
		for _, f := range late {
			if f.Status != "following" || f.Evidence != "measured" || f.Severity != "info" {
				t.Fatalf("steady confirmed charging read as %s/%s/%s (%s)", f.Status, f.Evidence, f.Severity, f.Reason)
			}
		}
	})
}

// A confirmed unchanged value after a new target is evidence too: a charger
// that keeps 11 kW when asked for 4 kW is not following.
func TestChangeOnlyChargerIgnoringNewTargetWarns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := telemetry.NewStore()
		tel.DriverHealthMut("cloud")
		srv := New(&Deps{Tel: tel})
		changedAt := time.Now()
		target := 11000.0
		var last ControlFeedback
		for sec := range 10 * 60 {
			if sec == 5*60 {
				target = 4000
			}
			if sec%60 == 0 || sec == 5*60 {
				c := tel.BeginCommand("cloud", []byte(fmt.Sprintf(`{"action":"ev_set_current","power_w":%g}`, target)), time.Now())
				tel.CompleteCommand(c, "accepted")
			}
			time.Sleep(time.Second)
			if sec%10 == 0 {
				data := fmt.Sprintf(`{"connected":true,"control_power_observed_at":%q,"control_power_confirmed":true,"power_max_age_s":180}`, changedAt.Format(time.RFC3339Nano))
				tel.Update("cloud", telemetry.DerEV, 11000, nil, []byte(data))
			}
			last = srv.controlFeedback(time.Now())[0]
		}
		if last.Status != "not_following" || last.Reason != "power_above_target" {
			t.Fatalf("ignored target read as %s/%s", last.Status, last.Reason)
		}
	})
}

// A tester's Easee load balancer cut an 11 kW charge to 8.3 kW while the
// battery charged. The charger named its load balancer, so the shortfall is a
// known limit, not an unexplained warning. Without that name it still warns,
// and more power than asked is never explained by a limit.
func TestLoadBalancedChargerReadsAsLimited(t *testing.T) {
	for _, tc := range []struct {
		name, extra              string
		actualW                  float64
		status, reason, severity string
	}{
		{"named", `,"current_limited_by":"load_balancer"`, 8300, "limited", "load_balancer_limit", "info"},
		{"held at zero", `,"current_limited_by":"load_balancer"`, 0, "limited", "load_balancer_limit", "info"},
		{"not named", ``, 8300, "not_following", "power_below_target", "warning"},
		{"above target", `,"current_limited_by":"load_balancer"`, 13000, "not_following", "power_above_target", "warning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := telemetry.NewStore()
				tel.DriverHealthMut("easee")
				srv := New(&Deps{Tel: tel})
				changedAt := time.Now()
				var last ControlFeedback
				for sec := range 6 * 60 {
					if sec%60 == 0 {
						c := tel.BeginCommand("easee", []byte(`{"action":"ev_set_current","power_w":11000}`), time.Now())
						tel.CompleteCommand(c, "accepted")
					}
					time.Sleep(time.Second)
					if sec%10 == 0 {
						data := fmt.Sprintf(`{"connected":true,"max_a":16,"control_power_observed_at":%q,"control_power_confirmed":true,"power_max_age_s":180%s}`,
							changedAt.Format(time.RFC3339Nano), tc.extra)
						tel.Update("easee", telemetry.DerEV, tc.actualW, nil, []byte(data))
					}
					last = srv.controlFeedback(time.Now())[0]
				}
				if last.Status != tc.status || last.Reason != tc.reason || last.Severity != tc.severity {
					t.Fatalf("got %s/%s/%s, want %s/%s/%s", last.Status, last.Reason, last.Severity, tc.status, tc.reason, tc.severity)
				}
			})
		})
	}
}
