package loadpoint

import (
	"context"
	"testing"
	"time"
)

func TestResumeAcknowledgementWaitsForMeasuredCharge(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11000, AllowedStepsW: []float64{0, 4140, 6900, 11000}}
	samples := map[string]EVSample{"easee": {Connected: true, RequestActive: true, DeviceID: "box", SessionID: "plug-1", ConnectionGeneration: 1}}
	dir := &Directive{SlotStart: now, SlotEnd: now.Add(time.Hour), LoadpointEnergyWh: map[string]float64{"garage": 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.Tick(context.Background(), now)
	dir.LoadpointEnergyWh["garage"] = 11000
	for _, seconds := range []int{5, 10, 35, 65, 95, 125} {
		c.Tick(context.Background(), now.Add(time.Duration(seconds)*time.Second))
	}
	if n := countAction(sender.calls, "ev_resume"); n != 3 {
		t.Fatalf("need bounded retries until delivery, got %d resumes: %+v", n, sender.calls)
	}
	sample := samples["easee"]
	sample.PowerW = 6900
	samples["easee"] = sample
	for _, seconds := range []int{130, 135, 140} {
		sample.PowerAt = now.Add(time.Duration(seconds) * time.Second)
		samples["easee"] = sample
		c.Tick(context.Background(), sample.PowerAt)
	}
	if _, exists := c.resumeOffers[cfg.ID]; exists {
		t.Fatal("sustained measured charging did not clear recovery proof")
	}
	sample.PowerW = 0
	samples["easee"] = sample
	c.Tick(context.Background(), now.Add(160*time.Second))
	if n := countAction(sender.calls, "ev_resume"); n != 3 {
		t.Fatalf("measured delivery must clear resume proof, got %d", n)
	}
}

func TestAcknowledgedResumeRetryRespectsSessionAndStops(t *testing.T) {
	for _, stop := range []string{"session", "safety", "plan_pause"} {
		t.Run(stop, func(t *testing.T) {
			now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
			sender := &fakeSender{}
			cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11000}
			samples := map[string]EVSample{"easee": {Connected: true, RequestActive: true, DeviceID: "box", SessionID: "one", ConnectionGeneration: 1}}
			dir := &Directive{SlotStart: now, SlotEnd: now.Add(time.Hour), LoadpointEnergyWh: map[string]float64{"garage": 0}}
			c := newTestController(t, []Config{cfg}, dir, samples, sender)
			c.Tick(context.Background(), now)
			dir.LoadpointEnergyWh["garage"] = 11000
			c.Tick(context.Background(), now.Add(5*time.Second))
			if countAction(sender.calls, "ev_resume") != 1 {
				t.Fatal(sender.calls)
			}
			switch stop {
			case "session":
				sample := samples["easee"]
				sample.SessionID = "two"
				sample.ConnectionGeneration = 2
				samples["easee"] = sample
			case "plan_pause":
				dir.LoadpointEnergyWh["garage"] = 0
			}
			c.TickWithDispatch(context.Background(), now.Add(35*time.Second), stop != "safety")
			if countAction(sender.calls, "ev_resume") != 1 {
				t.Fatalf("retry crossed %s: %+v", stop, sender.calls)
			}
		})
	}
}
