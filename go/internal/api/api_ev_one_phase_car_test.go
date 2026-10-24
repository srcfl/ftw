package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A one-phase car (a plug-in hybrid) on a three-phase charger, the field
// site's table and a plan for 4.8 kW, through a night. It takes the order's
// current per phase on L1. Once the meter shows that, the verdict says so
// and the car is never "not following" for doing what it can.
func TestOnePhaseCarThroughANight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := telemetry.NewStore()
		tel.DriverHealthMut("easee")
		mgr := loadpoint.NewManager()
		steps := []float64{0, 1600, 1900, 2100, 2400, 2600, 2800, 3100, 3300, 3500, 3800,
			4800, 5500, 6200, 6900, 7600, 8300, 9000, 9700, 10400, 11100}
		mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", MinChargeW: 1600, MaxChargeW: 11100, PhaseMode: "auto", AllowedStepsW: steps}})
		mgr.SetSchedule("garage", loadpoint.Schedule{SoC: .8})
		var orderW float64
		var orderAt time.Time
		drawW := func() float64 {
			if orderW <= 0 || time.Since(orderAt) < 40*time.Second {
				return 0
			}
			return math.Min(math.Floor(orderW/690+.5), 16) * 230
		}
		plan := func(now time.Time) (loadpoint.Directive, bool) {
			slot := now.Truncate(15 * time.Minute)
			return loadpoint.Directive{SlotStart: slot, SlotEnd: slot.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{"garage": 4800.0 / 4}}, true
		}
		sample := func(string) (loadpoint.EVSample, bool) {
			return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: drawW(), PowerAt: time.Now()}, true
		}
		send := func(_ context.Context, driver string, payload []byte) error {
			c := tel.BeginCommand(driver, payload, time.Now())
			tel.CompleteCommand(c, "accepted")
			var cmd struct {
				PowerW float64 `json:"power_w"`
			}
			if err := json.Unmarshal(payload, &cmd); err != nil {
				return err
			}
			if cmd.PowerW != orderW {
				orderW, orderAt = cmd.PowerW, time.Now()
			}
			return nil
		}
		ctrl := loadpoint.NewController(mgr, plan, sample, send)
		ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3})
		ctrl.SetSitePhases(func() (loadpoint.SitePhases, bool) {
			return loadpoint.SitePhases{A: [3]float64{1.6 + drawW()/230, 1.7, 2.3}, At: time.Now()}, true
		})
		srv := New(&Deps{Tel: tel, Loadpoints: mgr, Cfg: &config.Config{Fuse: config.Fuse{MaxAmps: 20, Voltage: 230, Phases: 3}}, CfgMu: &sync.RWMutex{}})
		changedAt := time.Now()
		last := -1.0
		var f ControlFeedback
		for range 8 * 3600 / 5 {
			ctrl.Tick(context.Background(), time.Now())
			if w := drawW(); w != last {
				last, changedAt = w, time.Now()
			}
			tel.Update("easee", telemetry.DerEV, drawW(), nil, []byte(fmt.Sprintf(
				`{"connected":true,"control_power_observed_at":%q,"control_power_confirmed":true,"power_max_age_s":180}`,
				changedAt.Format(time.RFC3339Nano))))
			time.Sleep(5 * time.Second)
			f = srv.controlFeedback(time.Now())[0]
			if f.Status == "not_following" {
				t.Fatalf("at %v a one-phase car drawing %.0f W of a %.0f W order reads %s/%s", time.Since(changedAt), drawW(), orderW, f.Status, f.Reason)
			}
		}
		st, _ := mgr.State("garage")
		if st.CarPhases != 1 || f.Reason != "one_phase" || f.Severity != "info" || f.ChargingPhase != "L1" ||
			f.ChargingPhaseMaxW == nil || *f.ChargingPhaseMaxW != 3680 || drawW() != 3680 {
			t.Fatalf("after a night: car phases %d, draw %.0f W, verdict %s/%s/%s on %q", st.CarPhases, drawW(), f.Status, f.Reason, f.Severity, f.ChargingPhase)
		}
	})
}
