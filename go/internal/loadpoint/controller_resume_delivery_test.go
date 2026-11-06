package loadpoint

import (
	"context"
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

func TestVehicleDeclinesChargeSuppressesResumeUntilRequestReturns(t *testing.T) {
	now := time.Now()
	cfg := Config{ID: "car", DriverName: "easee", MinChargeW: 4140}
	resumes := 0
	c := &Controller{send: func(context.Context, string, []byte) error { resumes++; return nil }}
	sample := EVSample{Connected: true, DeviceID: "box", SessionID: "one", RequestKnown: true, RequestActive: false, PowerAt: now}
	c.recoverOfferedCharge(context.Background(), cfg, sample, 0, now)
	for seconds := 1; seconds <= 300; seconds += 5 {
		sample.PowerAt = now.Add(time.Duration(seconds) * time.Second)
		c.recoverOfferedCharge(context.Background(), cfg, sample, 10350, sample.PowerAt)
	}
	if resumes != 0 {
		t.Fatalf("resumed stopped vehicle %d times", resumes)
	}
	if chargerIdleForWake(sample, sample.PowerAt) {
		t.Fatal("stopped vehicle eligible for wake")
	}
	sample.RequestActive = true
	for seconds := 305; seconds <= 460; seconds += 5 {
		sample.PowerAt = now.Add(time.Duration(seconds) * time.Second)
		c.recoverOfferedCharge(context.Background(), cfg, sample, 10350, sample.PowerAt)
	}
	if resumes == 0 {
		t.Fatal("new request did not restore failed-start retries")
	}
}
