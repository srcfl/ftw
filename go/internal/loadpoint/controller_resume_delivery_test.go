package loadpoint

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestResumeRequiresSustainedMeaningfulDelivery(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		power, offer            float64
		gap, stale, unavailable bool
		clear                   bool
	}{
		{name: "130 W pulse", power: 130, offer: 4830},
		{name: "800 W pulse", power: 800, offer: 4830},
		{name: "full power briefly", power: 4757, offer: 4830, gap: true},
		{name: "cached high sample", power: 4757, offer: 4830, stale: true},
		{name: "unknown high power", power: 4757, offer: 4830, unavailable: true},
		{name: "sustained charging", power: 4757, offer: 4830, clear: true},
		{name: "voltage tolerance at minimum", power: 3800, offer: 4140, clear: true},
		{name: "one phase offer", power: 1350, offer: 1380, clear: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			cfg := Config{ID: "car", DriverName: "easee", MinChargeW: 4140}
			resumes := 0
			c := &Controller{send: func(context.Context, string, []byte) error { resumes++; return nil }}
			sample := EVSample{DeviceID: "box", SessionID: "one", ConnectionGeneration: 1}
			c.recoverOfferedCharge(context.Background(), cfg, sample, 0, now)
			c.recoverOfferedCharge(context.Background(), cfg, sample, tc.offer, now.Add(time.Second))
			for _, seconds := range []int{5, 10, 15} {
				sample.PowerW = tc.power
				sample.PowerUnavailable = tc.unavailable
				sample.PowerAt = now.Add(time.Duration(seconds) * time.Second)
				if tc.stale {
					sample.PowerAt = now.Add(5 * time.Second)
				}
				if tc.gap && seconds == 10 {
					sample.PowerW = 0
				}
				c.recoverOfferedCharge(context.Background(), cfg, sample, tc.offer, now.Add(time.Duration(seconds)*time.Second))
			}
			_, exists := c.resumeOffers[cfg.ID]
			if exists == tc.clear {
				t.Fatalf("resume proof exists=%v want clear=%v", exists, tc.clear)
			}
			sample.PowerW = 0
			sample.PowerUnavailable = false
			sample.PowerAt = now.Add(35 * time.Second)
			c.recoverOfferedCharge(context.Background(), cfg, sample, tc.offer, now.Add(35*time.Second))
			want := 2
			if tc.clear {
				want = 1
			}
			if resumes != want {
				t.Fatalf("resumes=%v want=%v", resumes, want)
			}
		})
	}
}

// declineRig drives one loadpoint in tick order: observe the charger, record
// Core's order, then run recovery. The car does not ask for current.
type declineRig struct {
	m       *Manager
	c       *Controller
	cfg     Config
	sample  EVSample
	clock   time.Time
	mu      sync.Mutex
	actions []string
}

func newDeclineRig(t *testing.T) *declineRig {
	t.Helper()
	r := &declineRig{cfg: Config{ID: "car", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040},
		clock: time.Date(2026, 11, 6, 5, 43, 0, 0, time.UTC)}
	r.m = NewManager()
	r.m.Load([]Config{r.cfg})
	r.m.SetNowFn(func() time.Time { return r.clock })
	r.c = NewController(r.m, nil, nil, func(_ context.Context, _ string, payload []byte) error {
		var cmd struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(payload, &cmd)
		r.mu.Lock()
		r.actions = append(r.actions, cmd.Action)
		r.mu.Unlock()
		return nil
	})
	r.sample = EVSample{Connected: true, DeviceID: "box", SessionID: "one", RequestKnown: true, PowerAt: r.clock}
	r.m.SetCommanded(r.cfg.ID, 0, "pv_surplus_pause")
	return r
}

func (r *declineRig) tick(offerW float64) {
	r.sample.PowerAt = r.clock
	r.m.ObserveSample(r.cfg.ID, r.sample)
	r.m.SetCommanded(r.cfg.ID, offerW, "plan")
	r.c.recoverOfferedCharge(context.Background(), r.cfg, r.sample, offerW, r.clock)
	r.clock = r.clock.Add(5 * time.Second)
}

func (r *declineRig) count(action string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, a := range r.actions {
		if a == action {
			n++
		}
	}
	return n
}

// Easee reports reason 50 briefly during a session handshake, and after
// Core's own 0 A pause. A brief stop must not cancel the resume that pause
// needs; only a stop that outlasts SessionCompletionTimeout does.
func TestVehicleDeclineSuppressesResumeOnlyAfterItLasts(t *testing.T) {
	r := newDeclineRig(t)
	r.tick(0)
	r.tick(10350)
	if got := r.count("ev_resume"); got != 1 {
		t.Fatalf("brief stop after Core's pause: %d resumes, want 1", got)
	}
	start := r.clock
	for r.clock.Sub(start) < SessionCompletionTimeout {
		if r.c.vehicleDeclinesOffer(r.cfg.ID, r.sample, r.clock) {
			t.Fatalf("decline believed after %v", r.clock.Sub(start))
		}
		r.tick(10350)
	}
	for range 60 {
		r.tick(10350)
	}
	if !r.c.vehicleDeclinesOffer(r.cfg.ID, r.sample, r.clock) || r.c.resumeOffers[r.cfg.ID] != nil {
		t.Fatalf("lasting stop not believed: %+v", r.c.resumeOffers[r.cfg.ID])
	}
	attempts := r.count("ev_resume")
	r.tick(0)
	r.tick(10350) // Core's own pause again: one resume, as before.
	if got := r.count("ev_resume"); got != attempts+1 {
		t.Fatalf("resume after Core's pause: %d, want %d", got, attempts+1)
	}
	for range 60 {
		r.tick(10350)
	}
	if got := r.count("ev_resume"); got > attempts+3 {
		t.Fatalf("resumed a declining car %d times", got-attempts)
	}
	// Stale evidence cannot keep the stop.
	stale := r.sample
	stale.PowerAt = r.clock.Add(-time.Minute)
	if r.c.vehicleDeclinesOffer(r.cfg.ID, stale, r.clock) {
		t.Fatal("stale stop believed")
	}
	r.sample.RequestActive = true
	r.tick(10350)
	if r.c.vehicleDeclinesOffer(r.cfg.ID, r.sample, r.clock) {
		t.Fatal("decline kept after the car asked again")
	}
}

// A car woken from Stopped needs charge_start. A handshake stop must not
// block it; a stop that lasts while Core offers current does.
func TestVehicleWakeWaitsForALastingDecline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ticks int
		wake  bool
	}{
		{"brief stop", 3, true},
		{"lasting stop", 30, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeclineRig(t)
			r.c.SetVehicleStatus(func(string) (string, string, bool) { return "car-api", "Stopped", true })
			r.tick(10350)
			for range tc.ticks {
				r.tick(10350)
			}
			r.sample.PowerAt = r.clock
			r.c.maybeWakeVehicle(context.Background(), r.clock, r.cfg, false, r.sample, false, map[string]any{"power_w": 10350.0})
			if got := r.count("charge_start") == 1; got != tc.wake {
				t.Fatalf("charge_start sent=%v want %v", got, tc.wake)
			}
		})
	}
}

// A bare charger in "not requesting" after Core's own surplus pause gets a
// contactor cycle when Core offers current again.
func TestSelfPausedNotRequestingChargerStillGetsKick(t *testing.T) {
	oldGap := wallboxCycleGap
	wallboxCycleGap = 0
	t.Cleanup(func() { wallboxCycleGap = oldGap })
	r := newDeclineRig(t)
	r.tick(0)
	r.sample.PowerAt = r.clock
	r.c.maybeWakeVehicle(context.Background(), r.clock, r.cfg, true, r.sample, true, map[string]any{"power_w": 4140.0})
	deadline := time.Now().Add(2 * time.Second)
	for r.count("ev_resume") < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r.count("ev_pause") != 1 || r.count("ev_resume") != 1 {
		t.Fatalf("no contactor cycle: %d pause, %d resume", r.count("ev_pause"), r.count("ev_resume"))
	}
}
