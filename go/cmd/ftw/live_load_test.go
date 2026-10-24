package main

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func liveLoadStatus(t *testing.T, tel *telemetry.Store, ctrl *control.State, options func() telemetry.ForecastOptions) map[string]any {
	t.Helper()
	srv := api.New(&api.Deps{Tel: tel, Ctrl: ctrl, CtrlMu: &sync.Mutex{},
		Cfg: &config.Config{}, CfgMu: &sync.RWMutex{}, CapMu: &sync.RWMutex{},
		HistoryMeasurementSources: options})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest("GET", "/api/status", nil))
	if rr.Code != 200 {
		t.Fatal("status request failed")
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal("invalid status response")
	}
	return out
}

func liveLoadData(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal("invalid power evidence")
	}
	return data
}

func TestLiveLoadAppAndLANShareSourcePowerAndValidity(t *testing.T) {
	for _, scenario := range []string{"zero", "raw_not_smoothed", "site_side_ac", "missing_meter", "stale_power", "skew", "negative", "offline_ev", "idle_ev", "paused_ev", "faulted_ev"} {
		t.Run(scenario, func(t *testing.T) {
			tel := telemetry.NewStore()
			for _, name := range []string{"meter", "solar", "battery", "charger"} {
				tel.EnsureDriverHealth(name)
			}
			ctrl := &control.State{SiteMeterDriver: "meter", Mode: control.ModeSelfConsumption}
			tel.Update("meter", telemetry.DerMeter, 1000, nil, nil)
			want, valid := 1000.0, true
			switch scenario {
			case "zero":
				tel.Update("meter", telemetry.DerMeter, 0, nil, nil)
				want = 0
			case "raw_not_smoothed":
				tel.Update("meter", telemetry.DerMeter, 5000, nil, nil)
				want = 5000
				if tel.Get("meter", telemetry.DerMeter).SmoothedW == want {
					t.Fatal("test did not distinguish smoothed power")
				}
			case "site_side_ac":
				tel.Update("battery", telemetry.DerBattery, -2500, nil, liveLoadData(t, map[string]any{"control_power_w": -2000, "control_power_available": true}))
				want = 3000
			case "missing_meter":
				tel.Remove("meter")
				valid = false
			case "stale_power":
				tel.Update("meter", telemetry.DerMeter, 1000, nil, liveLoadData(t, map[string]any{"control_power_observed_at": time.Now().Add(-2 * time.Minute).Format(time.RFC3339Nano)}))
				valid = false
			case "skew":
				tel.Update("solar", telemetry.DerPV, -500, nil, liveLoadData(t, map[string]any{"control_power_observed_at": time.Now().Add(-45 * time.Second).Format(time.RFC3339Nano)}))
				want = 1500
			case "negative":
				tel.Update("battery", telemetry.DerBattery, 2000, nil, nil)
				want = -1000 // shown as is: production Core cannot see
			case "offline_ev":
				tel.Update("charger", telemetry.DerEV, 500, nil, nil)
				tel.DriverHealthMut("charger").SetOffline()
				// Left out, as before #1592: Home use is an estimate, not blank.
				want = 1000
			case "idle_ev", "paused_ev":
				fields := map[string]any{"connected": scenario == "paused_ev", "charging": false,
					"control_power_observed_at": time.Now().Add(-time.Hour).Format(time.RFC3339Nano),
					"control_power_available":   true, "control_power_confirmed": true}
				tel.Update("charger", telemetry.DerEV, 0, nil, liveLoadData(t, fields))
			case "faulted_ev":
				tel.Update("charger", telemetry.DerEV, 500, nil, nil)
				tel.SetDriverDeviceFault("charger", true, "command rejected")
				want = 500
			}
			site := &appSite{tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{}, revision: &control.Revision{}, started: time.Now(), siteMeterStale: time.Minute}
			snap := site.Snapshot()
			status := liveLoadStatus(t, tel, ctrl, nil)
			if snap.LoadWKnown != valid || (status["load_w"] != nil) != valid {
				t.Fatal("live adapters disagree on load validity")
			}
			if valid && (snap.LoadW != want || status["load_w"] != want) {
				t.Fatal("live adapters did not use the same source watts")
			}
			cloud := (&cloudSite{site: site}).Live()
			value, present := cloud.Fields[strconv.Itoa(int(appproto.FidLoadW))]
			if present != valid || valid && value != int64(math.Round(want)) {
				t.Fatal("Cloud changed load watts or validity")
			}
			if scenario == "site_side_ac" && (snap.BatteryW != -2000 || status["bat_w"] != float64(-2000)) {
				t.Fatal("live battery power differs from the AC balance")
			}
		})
	}
}

func TestLiveLoadOptionsFollowConfigurationForAppLANAndCloud(t *testing.T) {
	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("meter")
	tel.Update("meter", telemetry.DerMeter, 1000, nil, nil)
	ctrl := &control.State{SiteMeterDriver: "meter"}
	settings := &forecastSiteConfig{value: forecastSite{
		Options:        telemetry.ForecastOptions{HouseholdInvalidReason: "unconfirmed_battery_identity"},
		HistoryOptions: telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "battery", DerType: telemetry.DerBattery}}},
	}}
	options := func() telemetry.ForecastOptions { return settings.Snapshot().HistoryOptions }
	// Both link constructors use this constructor; Cloud uses the same appSite
	// adapter for its readable live copy as the encrypted phone session.
	site := newAppSite(tel, ctrl, &sync.Mutex{}, &control.Revision{}, time.Minute, options)
	for _, batteryDeclared := range []bool{true, false, true} {
		settings.mu.Lock()
		if batteryDeclared {
			settings.value.HistoryOptions.ExpectedFlows = []telemetry.ForecastFlow{{Driver: "battery", DerType: telemetry.DerBattery}}
		} else {
			settings.value.HistoryOptions.ExpectedFlows = nil
		}
		settings.mu.Unlock()
		snap := site.Snapshot()
		status := liveLoadStatus(t, tel, ctrl, options)
		_, cloudKnown := (&cloudSite{site: site}).Live().Fields[strconv.Itoa(int(appproto.FidLoadW))]
		// A declared battery that never reported is left out: Home use stays
		// known on every client and is marked estimated on the LAN.
		if !snap.LoadWKnown || status["load_w"] == nil || !cloudKnown {
			t.Fatal("configured never-emitted flow differs between clients")
		}
		wantQuality := map[bool]string{true: "estimated", false: "measured"}[batteryDeclared]
		if status["load_quality"] != wantQuality {
			t.Fatalf("load_quality = %v, want %s", status["load_quality"], wantQuality)
		}
		if snap.LoadW != 1000 || status["load_w"] != float64(1000) {
			t.Fatal("learning-only identity gate affected live load")
		}
		if batteryDeclared {
			found := false
			for _, source := range snap.Sources {
				found = found || source.ID == "battery"
			}
			if !found {
				t.Fatal("never-emitted source missing from app metadata")
			}
		}
	}
}

func TestLiveLoadWithoutConfiguredMeterIsUnavailable(t *testing.T) {
	tel := telemetry.NewStore()
	ctrl := &control.State{}
	site := newAppSite(tel, ctrl, &sync.Mutex{}, &control.Revision{}, time.Minute, nil)
	status := liveLoadStatus(t, tel, ctrl, nil)
	if _, present := status["load_w"]; !present || status["load_w"] != nil || status["grid_w"] != nil || site.Snapshot().LoadWKnown {
		t.Fatal("missing configuration became a measured zero")
	}
	if _, present := (&cloudSite{site: site}).Live().Fields[strconv.Itoa(int(appproto.FidLoadW))]; present {
		t.Fatal("Cloud invented load without a meter")
	}
}
