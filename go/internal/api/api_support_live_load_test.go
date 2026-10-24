package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func supportLoadStatus(t *testing.T, s *Server) map[string]any {
	t.Helper()
	s.deps.Cfg, s.deps.CfgMu, s.deps.CapMu = &config.Config{}, &sync.RWMutex{}, &sync.RWMutex{}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != 200 {
		t.Fatal("status request failed")
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal("invalid status response")
	}
	return out
}

func TestSupportLiveLoadMatchesLANSourcePowerAndKnownZero(t *testing.T) {
	for _, scenario := range []string{"raw_and_ac", "known_zero"} {
		t.Run(scenario, func(t *testing.T) {
			s, ctrl, tel := reportTestServer(t)
			want := 0.0
			if scenario == "raw_and_ac" {
				tel.Update("meter", telemetry.DerMeter, 5000, nil, nil)
				tel.EnsureDriverHealth("battery")
				tel.Update("battery", telemetry.DerBattery, -2500, nil, json.RawMessage(`{"control_power_w":-2000,"control_power_available":true}`))
				want = 7000
			} else {
				tel.Update("meter", telemetry.DerMeter, 0, nil, nil)
			}
			status := supportLoadStatus(t, s)
			snap := s.liveNow(*ctrl, time.Now())
			if status["load_w"] != want || snap.LoadW != want {
				t.Fatal("support and LAN disagree on qualified household watts")
			}
			var b strings.Builder
			writeRightNow(&b, *ctrl, snap, nil, nil, control.SlotEnergySnapshot{}, time.Now())
			if !strings.Contains(b.String(), "| House load | "+fmtReportW(want)+" |") {
				t.Fatal("known load did not render as a number")
			}
			if !snap.HaveLoad {
				t.Fatal("known zero lost load validity")
			}
			if scenario == "known_zero" {
				snap.PredictedLd = 5000
				found := false
				for _, f := range s.collectFindings(*ctrl, snap, nil, nil, nil, nil, control.SlotEnergySnapshot{}, time.Now()) {
					found = found || strings.Contains(f.Title, "load forecast")
				}
				if !found {
					t.Fatal("a real forecast miss against known zero was hidden")
				}
			}
		})
	}
}

func TestSupportUnknownLoadRendersUnavailableWithoutForecastClaims(t *testing.T) {
	for _, scenario := range []string{"stale_power"} {
		t.Run(scenario, func(t *testing.T) {
			s, ctrl, tel := reportTestServer(t)
			switch scenario {
			case "negative_balance":
				tel.EnsureDriverHealth("battery")
				tel.Update("battery", telemetry.DerBattery, 2000, nil, nil)
			case "declared_never_emitted":
				s.deps.HistoryMeasurementSources = func() telemetry.ForecastOptions {
					return telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "battery", DerType: telemetry.DerBattery}}}
				}
			case "stale_power":
				data, err := json.Marshal(map[string]any{"control_power_observed_at": time.Now().Add(-2 * time.Minute).Format(time.RFC3339Nano)})
				if err != nil {
					t.Fatal("invalid source time")
				}
				tel.Update("meter", telemetry.DerMeter, 500, nil, data)
			}
			if supportLoadStatus(t, s)["load_w"] != nil {
				t.Fatal("fixture did not make LAN household power unavailable")
			}
			snap := s.liveNow(*ctrl, time.Now())
			snap.PredictedLd = 5000
			active := &mpc.Action{LoadW: 5000, BatteryW: snap.BatW}
			var b strings.Builder
			writeRightNow(&b, *ctrl, snap, nil, nil, control.SlotEnergySnapshot{}, time.Now())
			writeForecastSection(&b, s, snap, active, time.Now())
			out := b.String()
			if !strings.Contains(out, "| House load | **unavailable** |") || !strings.Contains(out, "| Load the active slot assumed | 5.00 kW | **unavailable** |") {
				t.Fatal("unknown household load rendered as a watt value")
			}
			for _, f := range s.collectFindings(*ctrl, snap, nil, active, nil, nil, control.SlotEnergySnapshot{}, time.Now()) {
				if strings.Contains(f.Title, "load forecast") || strings.Contains(f.Title, "slot assumed a different house") {
					t.Fatal("unknown load caused a forecast or slot claim")
				}
			}
		})
	}
}

func TestSupportMissingFlowsDoNotCreatePowerMismatchClaims(t *testing.T) {
	s, ctrl, _ := reportTestServer(t)
	s.deps.HistoryMeasurementSources = func() telemetry.ForecastOptions {
		return telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{
			{Driver: "solar", DerType: telemetry.DerPV}, {Driver: "battery", DerType: telemetry.DerBattery},
		}}
	}
	snap := s.liveNow(*ctrl, time.Now())
	if snap.HaveLoad || snap.HavePV || snap.HaveBattery {
		t.Fatal("never-emitted flow became known")
	}
	snap.PredictedLd = 5000
	active := &mpc.Action{LoadW: 5000, PVW: -3000, BatteryW: 3000}
	var b strings.Builder
	writeRightNow(&b, *ctrl, snap, active, nil, control.SlotEnergySnapshot{}, time.Now())
	writeForecastSection(&b, s, snap, active, time.Now())
	out := b.String()
	if !strings.Contains(out, "| Solar | **unavailable** |") || !strings.Contains(out, "| Battery | **unavailable** |") || strings.Contains(out, "away from that intent") {
		t.Fatal("missing measurements became zero or a plan delivery claim")
	}
	for _, f := range s.collectFindings(*ctrl, snap, nil, active, nil, nil, control.SlotEnergySnapshot{}, time.Now()) {
		if strings.Contains(f.Title, "far from reality") || strings.Contains(f.Title, "slot assumed a different house") || strings.Contains(f.Title, "battery is not following") {
			t.Fatal("missing measurements caused a forecast or delivery claim")
		}
	}
}

func TestSupportUnknownLoadNeverBlamesEitherForecast(t *testing.T) {
	for _, activeOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "model", true: "active_slot"}[activeOnly], func(t *testing.T) {
			s, ctrl, _ := reportTestServer(t)
			s.deps.HistoryMeasurementSources = func() telemetry.ForecastOptions {
				return telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "battery", DerType: telemetry.DerBattery}}}
			}
			snap := s.liveNow(*ctrl, time.Now())
			snap.PredictedLd = 5000
			var active *mpc.Action
			if activeOnly {
				snap.PredictedLd = snap.LoadW
				active = &mpc.Action{LoadW: 5000}
			}
			for _, f := range s.collectFindings(*ctrl, snap, nil, active, nil, nil, control.SlotEnergySnapshot{}, time.Now()) {
				if strings.Contains(f.Title, "load forecast") || strings.Contains(f.Title, "slot assumed a different house") {
					t.Fatal("unknown load caused a forecast claim")
				}
			}
		})
	}
}

func TestSupportEstimatedLoadRendersEstimateWithoutForecastClaims(t *testing.T) {
	s, ctrl, _ := reportTestServer(t)
	s.deps.HistoryMeasurementSources = func() telemetry.ForecastOptions {
		return telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "battery", DerType: telemetry.DerBattery}}}
	}
	if st := supportLoadStatus(t, s); st["load_w"] == nil || st["load_quality"] != "estimated" {
		t.Fatal("left-out battery blanked LAN Home use", st)
	}
	snap := s.liveNow(*ctrl, time.Now())
	if snap.HaveLoad || !snap.LoadEstimated {
		t.Fatal("estimated load counted as measured")
	}
	snap.PredictedLd = 50000
	var b strings.Builder
	writeRightNow(&b, *ctrl, snap, nil, nil, control.SlotEnergySnapshot{}, time.Now())
	if !strings.Contains(b.String(), "(estimated: `missing:battery:battery`)") {
		t.Fatal("estimate not rendered as one", b.String())
	}
	for _, f := range s.collectFindings(*ctrl, snap, nil, nil, nil, nil, control.SlotEnergySnapshot{}, time.Now()) {
		if strings.Contains(f.Title, "load forecast") {
			t.Fatal("estimated load caused a forecast claim")
		}
	}
}
