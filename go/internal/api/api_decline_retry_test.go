package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// srcfl/ftw#2080 and its retry: a car that charges, becomes full and then
// refuses every offer is offered current about once an hour through a night.
// Each offer must end as a refusal, never as a fault, and the stop before
// the first refusal is not an interruption. Both charger readings under
// Core's own 0 A are covered: the car asking (Easee reason 52) and the car
// still not asking.
func TestFullCarRetriedThroughANightRaisesNoNotice(t *testing.T) {
	for _, askingUnderZero := range []bool{true, false} {
		t.Run(fmt.Sprintf("asking under 0 A=%v", askingUnderZero), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEVTrace(0)
				bus := events.NewBus()
				interrupted := 0
				bus.Subscribe(events.KindChargingInterrupted, func(events.Event) { interrupted++ })
				e.mgr.SetBus(bus)
				e.mgr.SetTarget("garage", 1, time.Now().Add(10*time.Hour))

				full := false
				offeredW := 0.0
				reading := func() (drawW float64, asking bool) {
					switch {
					case !full:
						return offeredW, true
					case offeredW >= loadpoint.DeliveringW:
						return 0, false
					}
					return 0, askingUnderZero
				}
				plan := loadpoint.PlanFunc(func(now time.Time) (loadpoint.Directive, bool) {
					slot := now.Truncate(15 * time.Minute)
					d := loadpoint.Directive{SlotStart: slot, SlotEnd: slot.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{}}
					if st, _ := e.mgr.State("garage"); !st.ChargingDeclined {
						d.LoadpointEnergyWh["garage"] = 2760
					}
					return d, true
				})
				sample := loadpoint.TelemetryFunc(func(string) (loadpoint.EVSample, bool) {
					drawW, asking := reading()
					return loadpoint.EVSample{Connected: true, PowerW: drawW, PowerAt: time.Now(), RequestKnown: true, RequestActive: asking}, true
				})
				send := loadpoint.SenderFunc(func(_ context.Context, _ string, payload []byte) error {
					var cmd struct {
						Action string  `json:"action"`
						PowerW float64 `json:"power_w"`
					}
					if json.Unmarshal(payload, &cmd) == nil && cmd.Action == "ev_set_current" {
						c := e.tel.BeginCommand("easee", payload, time.Now())
						e.tel.CompleteCommand(c, "accepted")
						offeredW = cmd.PowerW
					}
					return nil
				})
				ctrl := loadpoint.NewController(e.mgr, plan, sample, send)

				offers, wasOffered := 0, false
				start := time.Now()
				for time.Since(start) < 9*time.Hour {
					full = time.Since(start) >= 15*time.Minute
					drawW, asking := reading()
					extra := fmt.Sprintf(`,"request_active":%t`, asking)
					if !asking {
						extra += `,"op_mode":4`
					}
					e.tel.Update("easee", telemetry.DerEV, drawW, nil, []byte(fmt.Sprintf(
						`{"connected":true,"control_power_observed_at":%q,"control_power_confirmed":true,"power_max_age_s":180%s}`,
						time.Now().Format(time.RFC3339Nano), extra)))
					ctrl.Tick(context.Background(), time.Now())
					for _, st := range e.srv.ControlStates(time.Now()) {
						if st.Kind == "ev" && st.Status == "not_following" {
							t.Fatalf("at %v: %+v", time.Since(start), e.verdict(t))
						}
					}
					if offered := offeredW >= loadpoint.DeliveringW; offered && !wasOffered && full {
						offers++
					}
					wasOffered = offeredW >= loadpoint.DeliveringW
					time.Sleep(5 * time.Second)
				}
				if interrupted != 0 {
					t.Fatalf("%d charging.interrupted for a car that declined", interrupted)
				}
				if offers < 7 || offers > 9 {
					t.Fatalf("a night of refusals: %d offers, want about one an hour", offers)
				}
				if f := e.verdict(t); f.Reason != "vehicle_not_requesting" {
					t.Fatalf("between retries: %+v", f)
				}
			})
		})
	}
}
