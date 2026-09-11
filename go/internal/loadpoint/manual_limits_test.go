package loadpoint

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestManualSelectionIsAPowerCeiling(t *testing.T) {
	cfg := holdLoadpoint()
	for _, tc := range []struct{ request, max, want float64 }{
		{12000, 11000, 11000}, {5500, 11000, 4830}, {11000, 7000, 6900},
		{1000, 11000, 0}, {0, 11000, 0}, {math.NaN(), 11000, 0},
	} {
		cfg.MaxChargeW = tc.max
		got := clampManualPower(cfg, ManualHold{PowerW: tc.request, PhaseMode: "3p"}, SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
		if got != tc.want {
			t.Errorf("request %v max %v: got %v want %v", tc.request, tc.max, got, tc.want)
		}
	}
}

func TestPauseHoldsThroughCarRefusalAndResumesOnlyOnRequest(t *testing.T) {
	now := time.Now()
	cfg := overrideLoadpoint()
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: false}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 0, Persistent: true})
	for _, dt := range []time.Duration{0, time.Minute, 3 * time.Minute, time.Hour} {
		c.Tick(context.Background(), now.Add(dt))
		if h, ok := c.GetManualHold(cfg.ID, now.Add(dt)); !ok || h.PowerW != 0 {
			t.Fatalf("pause was released at %v: %+v %v", dt, h, ok)
		}
		if len(sender.calls) == 0 || sender.calls[len(sender.calls)-1].power != 0 {
			t.Fatal("pause ordered nonzero power")
		}
	}
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true})
	c.Tick(context.Background(), now.Add(time.Hour+time.Second))
	if got := sender.calls[len(sender.calls)-1].power; got != 4140 {
		t.Fatalf("explicit start failed without planner: %v", got)
	}
}

func TestSnapStepsCannotExceedChargerRating(t *testing.T) {
	if got := SnapChargeW(5000, 1380, 5000, []float64{0, 4140, 5520}); got != 4140 {
		t.Fatalf("unsafe step above maximum: %v", got)
	}
	if got := SnapChargeW(2000, 1380, 2000, []float64{0, 4140}); got != 0 {
		t.Fatalf("no valid step must stop: %v", got)
	}
}

func TestCurrentCeilingCannotTurnZeroFuseBudgetIntoDefaultAmps(t *testing.T) {
	for _, tc := range []struct{ amps, want float64 }{{0, 0}, {5.5, 0}, {8.9, 5520}, {16, 11000}} {
		cmd := map[string]any{"power_w": float64(11000), "voltage": float64(230), "phase_mode": "3p", "max_amps_per_phase": tc.amps}
		applyCurrentCeiling(cmd)
		if got := cmd["power_w"].(float64); got != tc.want {
			t.Fatalf("ceiling %vA sent %vW, want %vW", tc.amps, got, tc.want)
		}
	}
}

// A fuse budget is a ceiling, including between steps and below the minimum.
// The dispatch may pause; it must never increase an already reduced request.
func TestFusePowerBudgetNeverRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		want, cap, max, expected float64
		steps                    []float64
	}{
		{"below minimum", 11000, 3000, 11000, 0, []float64{0, 4140, 6900, 11000}},
		{"between steps", 11000, 6000, 11000, 4140, []float64{0, 4140, 6900, 11000}},
		{"no steps below minimum", 11000, 3000, 11000, 0, nil},
		{"continuous budget", 11000, 5000, 11000, 5000, nil},
		{"invalid step above rating", 11000, 8000, 7000, 6900, []float64{0, 4140, 6900, 8000}},
		{"reduced one phase request", 2000, 1500, 11000, 0, []float64{0, 4140, 6900, 11000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := holdLoadpoint()
			cfg.MinChargeW = 4140
			cfg.MaxChargeW = tc.max
			cfg.AllowedStepsW = tc.steps
			c := newTestController(t, []Config{cfg}, nil, map[string]EVSample{}, &fakeSender{})
			c.SetFuseEVMax(func() (float64, bool) { return tc.cap, true })
			got, _ := c.applyFuseClampAndCooldown(time.Now(), cfg, tc.want)
			if got != tc.expected || got > tc.cap || got > tc.want {
				t.Fatalf("want %v cap %v: sent %v, expected %v", tc.want, tc.cap, got, tc.expected)
			}
		})
	}
}
