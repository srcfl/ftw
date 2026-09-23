package loadpoint

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// A 0 W offer (no plan, surplus pause, safety standdown) maps on Easee to a
// sticky user-pause at 0 A. The next non-zero offer must send ev_resume
// before ev_set_current, otherwise the contactor stays open until unplug.
func TestTickResumesChargerWhenOfferReturns(t *testing.T) {
	sender := &fakeSender{}
	cfgs := []Config{{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1400,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1400, 4100, 7400, 11000},
	}}
	samples := map[string]EVSample{
		"easee": {PowerW: 0, Connected: true, RequestActive: true},
	}
	m := NewManager()
	m.Load(cfgs)
	var dir *Directive
	plan := PlanFunc(func(now time.Time) (Directive, bool) {
		if dir == nil {
			return Directive{}, false
		}
		return *dir, true
	})
	tel := TelemetryFunc(func(driver string) (EVSample, bool) {
		s, ok := samples[driver]
		return s, ok
	})
	c := NewController(m, plan, tel, sender.Send)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	c.Tick(context.Background(), now)
	if len(sender.calls) != 1 || sender.calls[0].action != "ev_set_current" || sender.calls[0].power != 0 {
		t.Fatalf("no-plan standdown: %+v", sender.calls)
	}
	if countAction(sender.calls, "ev_resume") != 0 {
		t.Fatalf("first 0 W command must not resume: %+v", sender.calls)
	}

	dir = &Directive{
		SlotStart:         now,
		SlotEnd:           now.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{"garage": 2750},
	}
	c.Tick(context.Background(), now.Add(5*time.Second))
	if len(sender.calls) != 3 {
		t.Fatalf("reoffer command count = %d, want 3 (0 W, resume, set); got %+v", len(sender.calls), sender.calls)
	}
	if sender.calls[1].action != "ev_resume" {
		t.Fatalf("expected ev_resume after 0 W standdown, got %+v", sender.calls[1])
	}
	if sender.calls[2].action != "ev_set_current" || sender.calls[2].power != 11000 {
		t.Fatalf("expected 11000 W ev_set_current after resume, got %+v", sender.calls[2])
	}

	sender.calls = nil
	c.Tick(context.Background(), now.Add(10*time.Second))
	if countAction(sender.calls, "ev_resume") != 0 {
		t.Fatalf("must not resume again while already offering: %+v", sender.calls)
	}
	set, ok := lastSetCurrent(sender.calls)
	if !ok || set.power != 11000 {
		t.Fatalf("continued offer: %+v", sender.calls)
	}
}

func TestTickFirstPositiveOfferDoesNotResume(t *testing.T) {
	sender := &fakeSender{}
	slotStart := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	cfgs := []Config{{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1400,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1400, 4100, 7400, 11000},
	}}
	directive := &Directive{
		SlotStart:         slotStart,
		SlotEnd:           slotStart.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{"garage": 2750},
	}
	samples := map[string]EVSample{"easee": {PowerW: 0, Connected: true, RequestActive: true}}
	c := newTestController(t, cfgs, directive, samples, sender)

	c.Tick(context.Background(), slotStart)
	if len(sender.calls) != 1 || sender.calls[0].action != "ev_set_current" || sender.calls[0].power != 11000 {
		t.Fatalf("first offer of a process: %+v", sender.calls)
	}
	if countAction(sender.calls, "ev_resume") != 0 {
		t.Fatalf("first offer is not a standdown we issued: %+v", sender.calls)
	}
}

func TestTickRetriesFailedResumeWithBackoff(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	cfgs := []Config{{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11000}}
	samples := map[string]EVSample{"easee": {Connected: true, RequestActive: true, DeviceID: "charger", SessionID: "plug-1", ConnectionGeneration: 1}}
	dir := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{"garage": 0}}
	c := newTestController(t, cfgs, dir, samples, sender)
	attempts := 0
	outcomes := 0
	c.SetDispatchOutcome(func(_ string, err error, _ time.Time) {
		outcomes++
		if err != nil {
			t.Fatalf("optional resume changed driver health: %v", err)
		}
	})
	c.send = func(ctx context.Context, driver string, payload []byte) error {
		var cmd struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(payload, &cmd); err != nil {
			return err
		}
		if cmd.Action == "ev_resume" {
			attempts++
			if attempts < 3 {
				return errors.New("temporary transport failure")
			}
		}
		return sender.Send(ctx, driver, payload)
	}
	c.Tick(context.Background(), now)
	dir.LoadpointEnergyWh["garage"] = 2750
	for _, seconds := range []int{5, 6, 9, 10, 15, 19} {
		c.Tick(context.Background(), now.Add(time.Duration(seconds)*time.Second))
	}
	if attempts != 2 {
		t.Fatalf("got %d resume attempts before second backoff elapsed, want 2", attempts)
	}
	c.Tick(context.Background(), now.Add(20*time.Second))
	c.Tick(context.Background(), now.Add(25*time.Second))
	if attempts != 3 {
		t.Fatalf("failed resume was lost or successful resume repeated: %d", attempts)
	}
	if outcomes != 9 {
		t.Fatalf("current dispatch must continue during optional resume failures: %d", outcomes)
	}
	st, _ := c.manager.State("garage")
	if st.CommandedW <= 0 || st.CurrentPowerW != 0 {
		t.Fatalf("request must remain distinct from actual power: %+v", st)
	}
}

func TestTickCancelsResumeRetryWhenSessionProofChanges(t *testing.T) {
	for _, change := range []string{"unplug", "session", "device", "generation", "unknown", "delivering"} {
		t.Run(change, func(t *testing.T) {
			now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
			sender := &fakeSender{}
			cfgs := []Config{{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11000}}
			sample := EVSample{Connected: true, RequestActive: true, DeviceID: "charger", SessionID: "plug-1", ConnectionGeneration: 1}
			samples := map[string]EVSample{"easee": sample}
			dir := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{"garage": 0}}
			c := newTestController(t, cfgs, dir, samples, sender)
			attempts := 0
			c.send = func(ctx context.Context, driver string, payload []byte) error {
				var cmd struct {
					Action string `json:"action"`
				}
				_ = json.Unmarshal(payload, &cmd)
				if cmd.Action == "ev_resume" {
					attempts++
					return errors.New("temporary failure")
				}
				return sender.Send(ctx, driver, payload)
			}
			c.Tick(context.Background(), now)
			dir.LoadpointEnergyWh["garage"] = 2750
			c.Tick(context.Background(), now.Add(5*time.Second))
			switch change {
			case "unplug":
				sample.Connected = false
			case "session":
				sample.SessionID = "plug-2"
			case "device":
				sample.DeviceID = "other-charger"
			case "generation":
				sample.ConnectionGeneration++
			case "unknown":
				sample.ConnectionUnknown = true
			case "delivering":
				sample.PowerW = 5000
			}
			samples["easee"] = sample
			c.Tick(context.Background(), now.Add(10*time.Second))
			sample.Connected, sample.ConnectionUnknown, sample.PowerW = true, false, 0
			samples["easee"] = sample
			c.Tick(context.Background(), now.Add(15*time.Second))
			if attempts != 1 {
				t.Fatalf("stale retry crossed %s boundary: %d attempts", change, attempts)
			}
		})
	}
}

func TestFailedZeroCommandDoesNotAuthorizeResume(t *testing.T) {
	for _, safetyStanddown := range []bool{false, true} {
		now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
		cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11000}
		dir := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{"garage": 0}}
		c := newTestController(t, []Config{cfg}, dir, map[string]EVSample{"easee": {Connected: true, RequestActive: true}}, &fakeSender{})
		resumes := 0
		c.send = func(_ context.Context, _ string, payload []byte) error {
			var cmd struct {
				Action string  `json:"action"`
				PowerW float64 `json:"power_w"`
			}
			if err := json.Unmarshal(payload, &cmd); err != nil {
				return err
			}
			if cmd.Action == "ev_resume" {
				resumes++
				return nil
			}
			if cmd.PowerW == 0 {
				return errors.New("zero command rejected")
			}
			return nil
		}
		c.TickWithDispatch(context.Background(), now, !safetyStanddown)
		dir.LoadpointEnergyWh["garage"] = 2750
		c.Tick(context.Background(), now.Add(5*time.Second))
		if resumes != 0 {
			t.Fatal("failed standdown was treated as an acknowledged pause")
		}
	}
}
