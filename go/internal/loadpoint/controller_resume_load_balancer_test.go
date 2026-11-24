package loadpoint

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Field report, srcfl/ftw#1948: an Easee Equalizer held the start for 11–14
// minutes after FTW resumed the charger, and Core logged a failed start after
// its three resumes, about two minutes in. With the driver's word that the
// charger's own load balancing holds the start, Core waits up to
// LoadBalancerRaiseWait before it logs the failure; without it, or once the
// hold ends, a start that has not come has failed.
func TestResumeWarningWaitsOutALoadBalancerHold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hold    bool
		release time.Duration // when the driver stops reporting the hold; 0 never
		warnAt  time.Duration // when the failed start is logged, from the resume
	}{
		{"no hold", false, 0, 90 * time.Second},
		{"held throughout", true, 0, LoadBalancerRaiseWait},
		{"hold ends without a start", true, 5 * time.Minute, 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			now := time.Date(2026, 11, 12, 2, 24, 0, 0, time.UTC)
			sender := &fakeSender{}
			cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11000, AllowedStepsW: []float64{0, 4140, 8280, 11000}}
			samples := map[string]EVSample{"easee": {Connected: true, RequestActive: true, DeviceID: "box", SessionID: "plug-1",
				ConnectionGeneration: 1, LoadBalancerHold: tc.hold}}
			dir := &Directive{SlotStart: now, SlotEnd: now.Add(time.Hour), LoadpointEnergyWh: map[string]float64{"garage": 0}}
			c := newTestController(t, []Config{cfg}, dir, samples, sender)
			c.Tick(context.Background(), now)
			dir.LoadpointEnergyWh["garage"] = 8280
			resumed := now.Add(5 * time.Second)
			warned := time.Duration(0)
			for at := resumed; at.Before(resumed.Add(20 * time.Minute)); at = at.Add(5 * time.Second) {
				if tc.release > 0 && at.Sub(resumed) >= tc.release {
					sample := samples["easee"]
					sample.LoadBalancerHold = false
					samples["easee"] = sample
				}
				c.Tick(context.Background(), at)
				if warned == 0 && strings.Contains(logs.String(), "no charge observed") {
					warned = at.Sub(resumed)
				}
			}
			if n := countAction(sender.calls, "ev_resume"); n != 3 {
				t.Fatalf("resumes: %d, want 3 whether or not the start is held", n)
			}
			if warned < tc.warnAt || warned > tc.warnAt+10*time.Second {
				t.Fatalf("failed start logged %v after the resume, want %v\n%s", warned, tc.warnAt, logs.String())
			}
			if n := strings.Count(logs.String(), "no charge observed"); n != 1 {
				t.Fatalf("failed start logged %d times", n)
			}
			if tc.hold && !strings.Contains(logs.String(), "load_balancer_hold=true") {
				t.Fatalf("resume log does not name the hold:\n%s", logs.String())
			}
		})
	}
}
