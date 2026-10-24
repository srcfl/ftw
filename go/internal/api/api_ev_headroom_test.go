package api

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"sync"
	"testing"
	"time"
)

func TestEVSiteHeadroomProjection(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		modify func(*ControlFeedback, *config.Fuse, *[]float64, *int, *time.Time)
		want   bool
	}{
		{"actual household case", nil, true},
		{"ample headroom", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) { *a = []float64{8, 9, 8} }, false},
		{"asymmetric busiest phase", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) {
			*a = []float64{10, 21, 10}
		}, true},
		{"stale meter", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) {
			*at = now.Add(-16 * time.Second)
		}, false},
		{"future meter", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) {
			*at = now.Add(time.Second)
		}, false},
		{"misaligned EV", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) {
			f.ObservedAtMs = now.Add(-11 * time.Second).UnixMilli()
		}, false},
		{"stale EV", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) { f.ReadingsFresh = false }, false},
		{"single phase mapping unknown", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) { *p = 1 }, false},
		{"missing phase", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) { *a = []float64{21, 20} }, false},
		{"nonfinite phase", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) { (*a)[0] = math.NaN() }, false},
		{"mixed export", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) { (*a)[0] = -3 }, false},
		{"failed command precedence", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) {
			f.Reason = "command_failed"
		}, false},
		{"device reason precedence", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) {
			f.Reason = "load_balancer_limit"
		}, false},
		{"one phase reason precedence", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) { f.Reason = "one_phase" }, false},
		{"hourly import reason precedence", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) {
			f.Reason = "import_limit"
		}, false},
		{"zero delivery not explained", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) { f.ActualW = watts(0) }, false},
		{"invalid safety margin", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) {
			c.SafetyMarginA = watts(26)
		}, false},
		{"safety margin binds", func(f *ControlFeedback, c *config.Fuse, a *[]float64, p *int, at *time.Time) {
			c.SafetyMarginA = watts(2)
			*a = []float64{15, 15, 15}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ControlFeedback{Kind: "ev", Reason: "power_below_target", ReadingsFresh: true, SentW: watts(10350), ActualW: watts(4155), ObservedAtMs: now.UnixMilli()}
			c := config.Fuse{MaxAmps: 25, Phases: 3, Voltage: 230}
			a := []float64{19.65, 20.52, 19.46}
			p := 3
			at := now
			if tc.modify != nil {
				tc.modify(&f, &c, &a, &p, &at)
			}
			got := projectEVHeadroom(f, p, 230, c, a, []float64{4519.5, 4719.6, 4475.8}, at, now)
			if (got != nil) != tc.want {
				t.Fatalf("projection=%+v want present=%v", got, tc.want)
			}
			if got != nil && tc.name == "actual household case" && math.Abs(got.ProjectedMaxA-29.498) > 0.01 {
				t.Fatalf("unexpected projection %+v", got)
			}
		})
	}
}

func TestEVSiteHeadroomServerContext(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("meter", telemetry.DerMeter, 13843, nil, []byte(`{"l1_a":19.65,"l2_a":20.52,"l3_a":19.46,"l1_w":4519.5,"l2_w":4719.6,"l3_w":4475.8}`))
	tel.Update("battery", telemetry.DerBattery, 9383, nil, nil)
	tel.DriverHealthMut("meter").RecordSuccess()
	tel.DriverHealthMut("battery").RecordSuccess()
	cfg := &config.Config{Fuse: config.Fuse{MaxAmps: 25, Phases: 3, Voltage: 230}, Loadpoints: []config.Loadpoint{{DriverName: "ev", PhaseMode: "3p"}}}
	srv := New(&Deps{Tel: tel, Cfg: cfg, CfgMu: &sync.RWMutex{}})
	now := time.Now()
	f := ControlFeedback{Driver: "ev", Kind: "ev", Reason: "power_below_target", ReadingsFresh: true, SentW: watts(10350), ActualW: watts(4155), ObservedAtMs: now.UnixMilli()}
	lp := loadpoint.State{Phases: 3, VoltageV: 230}
	got := srv.evSiteHeadroom(f, lp, "meter", now)
	if got == nil || got.BatteryChargeW != 9383 {
		t.Fatalf("missing site/battery context %+v", got)
	}
	tel.DriverHealthMut("battery").SetDeviceFault(true, "test")
	got = srv.evSiteHeadroom(f, lp, "meter", now)
	if got == nil || got.BatteryChargeW != 0 {
		t.Fatalf("faulted battery contributed %+v", got)
	}
	cfg.Loadpoints[0].PhaseMode = "auto"
	if got = srv.evSiteHeadroom(f, lp, "meter", now); got != nil {
		t.Fatalf("unknown phase mapping %+v", got)
	}
	cfg.Loadpoints[0].PhaseMode = "3p"
	tel.DriverHealthMut("meter").SetDeviceFault(true, "test")
	if got = srv.evSiteHeadroom(f, lp, "meter", now); got != nil {
		t.Fatalf("faulted meter contributed %+v", got)
	}
}

func TestEVSiteHeadroomRejectsInvalidPhasePower(t *testing.T) {
	now := time.Now()
	f := ControlFeedback{Kind: "ev", Reason: "power_below_target", ReadingsFresh: true, SentW: watts(10350), ActualW: watts(4155), ObservedAtMs: now.UnixMilli()}
	fuse := config.Fuse{MaxAmps: 25, Phases: 3, Voltage: 230}
	for _, powers := range [][]float64{nil, {4600, 4600}, {math.NaN(), 4600, 4600}, {4600, math.Inf(1), 4600}, {4600, 4600, math.Inf(-1)}} {
		if got := projectEVHeadroom(f, 3, 230, fuse, []float64{20, 20, 20}, powers, now, now); got != nil {
			t.Fatalf("invalid phase power produced explanation: %+v", got)
		}
	}
}
