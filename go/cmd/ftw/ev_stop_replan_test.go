package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestEVStopReplanOnlyWhenTheCarStoppedItself(t *testing.T) {
	charging := loadpoint.State{PluggedIn: true, CurrentPowerW: 0, CommandedKnown: true, CommandedW: 6900}
	cases := []struct {
		name  string
		state loadpoint.State
		prevW float64
		want  bool
	}{
		{"car stopped while Core offered power", charging, 6900, true},
		{"Core ordered the stop", loadpoint.State{PluggedIn: true, CommandedKnown: true, CommandedW: 0}, 6900, false},
		{"no command known yet", loadpoint.State{PluggedIn: true}, 6900, true},
		{"was not charging", charging, 20, false},
		{"still charging", loadpoint.State{PluggedIn: true, CurrentPowerW: 4140, CommandedKnown: true, CommandedW: 4140}, 6900, false},
		{"unplugged", loadpoint.State{CommandedKnown: true, CommandedW: 6900}, 6900, false},
	}
	for _, tc := range cases {
		if got := evStopNeedsReplan(tc.state, tc.prevW); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The plan follows the refusal latch at once: it leaves a car out as soon as
// the car has declined, and takes it in as soon as the latch ends while the
// car is still plugged in (srcfl/ftw#2080). An unplug replans on its own.
func TestDeclineEdgesReplan(t *testing.T) {
	for _, tc := range []struct {
		name               string
		prev, now, plugged bool
		want               string
	}{
		{"car declined", false, true, true, "loadpoint_declined"},
		{"refusal ended", true, false, true, "loadpoint_decline_ended"},
		{"unplugged", true, false, false, ""},
		{"still declined", true, true, true, ""},
		{"never declined", false, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lp := loadpoint.State{PluggedIn: tc.plugged, ChargingDeclined: tc.now}
			if got := declineReplanReason(lp, tc.prev); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// The decline retry asks the real registry: an observe-only or read-only
// charger is never offered current again on its own, and one Core controls
// is (srcfl/ftw#2080).
func TestDeclineRetryFollowsTheRegistrysControlAuthority(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		observeOnly, readOnly bool
		retry                 bool
	}{
		{"controlled", false, false, true},
		{"observe only", true, false, false},
		{"read only", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := drivers.NewRegistry(telemetry.NewStore())
			t.Cleanup(reg.ShutdownAll)
			lua := filepath.Join(t.TempDir(), "charger.lua")
			readOnly := "false"
			if tc.readOnly {
				readOnly = "true"
			}
			src := `DRIVER = { id = "charger", capabilities = { "ev" }, read_only = ` + readOnly + ` }
function driver_init(config) end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
function driver_default_mode() end
`
			if err := os.WriteFile(lua, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := reg.Add(context.Background(), config.Driver{Name: "easee", Lua: lua, ObserveOnly: tc.observeOnly}); err != nil {
				t.Fatal(err)
			}

			clock := time.Date(2026, 11, 24, 22, 0, 0, 0, time.UTC)
			mgr := loadpoint.NewManager()
			mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040}})
			mgr.SetNowFn(func() time.Time { return clock })
			mgr.SetTarget("garage", 1, clock.Add(9*time.Hour))
			offeredW := 0.0
			plan := loadpoint.PlanFunc(func(now time.Time) (loadpoint.Directive, bool) {
				slot := now.Truncate(15 * time.Minute)
				d := loadpoint.Directive{SlotStart: slot, SlotEnd: slot.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{}}
				if st, _ := mgr.State("garage"); !st.ChargingDeclined {
					d.LoadpointEnergyWh["garage"] = 2760
				}
				return d, true
			})
			// A full car: it refuses every offer; under Core's 0 A the
			// charger reports it as asking, as an Easee does.
			tel := loadpoint.TelemetryFunc(func(string) (loadpoint.EVSample, bool) {
				return loadpoint.EVSample{Connected: true, RequestActive: offeredW < loadpoint.DeliveringW}, true
			})
			send := loadpoint.SenderFunc(func(_ context.Context, _ string, payload []byte) error {
				var cmd struct {
					Action string  `json:"action"`
					PowerW float64 `json:"power_w"`
				}
				if json.Unmarshal(payload, &cmd) == nil && cmd.Action == "ev_set_current" {
					offeredW = cmd.PowerW
				}
				return nil
			})
			ctrl := loadpoint.NewController(mgr, plan, tel, send)
			ctrl.SetChargerControlAllowed(chargerControlAllowed(reg, nil))

			declinedAt := time.Time{}
			for end := clock.Add(loadpoint.DeclineRetryInterval + 10*time.Minute); clock.Before(end); clock = clock.Add(5 * time.Second) {
				ctrl.Tick(context.Background(), clock)
				st, _ := mgr.State("garage")
				if declinedAt.IsZero() && st.ChargingDeclined {
					declinedAt = clock
				}
				if !declinedAt.IsZero() && !st.ChargingDeclined {
					if !tc.retry {
						t.Fatalf("retried at %v", clock.Sub(declinedAt))
					}
					if clock.Sub(declinedAt) < loadpoint.DeclineRetryInterval {
						t.Fatalf("retried %v after the refusal", clock.Sub(declinedAt))
					}
					return
				}
			}
			if declinedAt.IsZero() {
				t.Fatal("the car's refusal never latched")
			}
			if tc.retry {
				t.Fatal("a charger Core controls was not retried")
			}
		})
	}
}
