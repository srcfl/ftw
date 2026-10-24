package api

import (
	"context"
	"encoding/json"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestPartialHistoryAPIKeepsKnownFlowsAndNullsMissingValues(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute).UnixMilli()
	for i := 0; i < 4; i++ {
		p := state.HistoryPoint{TsMs: now + int64(i*10000), GridW: 1200, PVW: -300, Missing: state.MissingBattery | state.MissingLoad | state.MissingSoC, JSON: `{"drivers":{"meter":{"meter_w":1200},"battery":{"bat_w":null}}}`}
		if err := st.EnqueueTelemetryTick(&p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{State: st})
	rr := httptest.NewRecorder()
	srv.handleHistory(rr, httptest.NewRequest("GET", "/api/history?range=5m&points=1", nil))
	var h struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if len(h.Items) != 1 {
		t.Fatal(rr.Body.String())
	}
	p := h.Items[0]
	if p["grid_w"] != float64(1200) || p["pv_w"] != float64(-300) || p["bat_w"] != nil || p["bat_soc"] != nil || p["load_w"] != nil || p["history_quality"] != "incomplete" {
		t.Fatal(p)
	}
	rr = httptest.NewRecorder()
	srv.handleEnergyDaily(rr, httptest.NewRequest("GET", "/api/energy/daily?days=7", nil))
	var d struct {
		Days []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	today := d.Days[len(d.Days)-1]
	if math.Abs(today["import_wh"].(float64)-10) > 1e-9 || math.Abs(today["pv_wh"].(float64)-2.5) > 1e-9 || today["load_wh"] != nil || today["bat_charged_wh"] != nil || today["history_quality"] != "incomplete" {
		t.Fatal(today)
	}
	if d.Days[0]["import_wh"] != nil {
		t.Fatal("empty day shown as measured zero", d.Days[0])
	}
}

func TestLiveHistoryQualitySeparatesOfflineFromInvalidBalance(t *testing.T) {
	tel := telemetry.NewStore()
	for _, name := range []string{"meter", "battery"} {
		tel.EnsureDriverHealth(name)
	}
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	tel.RecordDriverSuccess("meter")
	tel.DriverHealthMut("battery").SetOffline()
	srv := New(&Deps{Tel: tel, HistoryMeasurementSources: func() telemetry.ForecastOptions {
		return telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "battery", DerType: telemetry.DerBattery}}}
	}})
	resp := map[string]any{}
	srv.qualifyStatusHistory(resp, "meter")
	// Live Home use leaves the offline battery out; history stays incomplete.
	if resp["grid_w"] != float64(1200) || resp["bat_w"] != nil || resp["load_w"] != float64(1200) || resp["load_quality"] != "estimated" || resp["history_quality"] != "incomplete" {
		t.Fatal(resp)
	}
	tel.Update("battery", telemetry.DerBattery, 2000, nil, nil)
	tel.RecordDriverSuccess("battery")
	srv.qualifyStatusHistory(resp, "meter")
	// Negative balance: shown as is, history still records the mismatch.
	if resp["bat_w"] != float64(2000) || resp["load_w"] != float64(-800) || resp["load_reason"] != "invalid_balance" || resp["history_quality"] != "balance_mismatch" {
		t.Fatal(resp)
	}
	tel.Update("battery", telemetry.DerBattery, 0, nil, nil)
	tel.RecordDriverSuccess("battery")
	srv.qualifyStatusHistory(resp, "meter")
	if resp["bat_w"] != float64(0) || resp["load_w"] != float64(1200) || resp["history_quality"] != "complete" {
		t.Fatal(resp)
	}
}

func TestLiveHistoryQualityKeepsWeightedSoCAndClearsStaleDriverSoC(t *testing.T) {
	tel := telemetry.NewStore()
	for _, name := range []string{"meter", "large", "small"} {
		tel.EnsureDriverHealth(name)
	}
	low, high := .2, .8
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	tel.RecordDriverSuccess("meter")
	tel.Update("large", telemetry.DerBattery, 100, &low, nil)
	tel.RecordDriverSuccess("large")
	tel.Update("small", telemetry.DerBattery, 100, &high, nil)
	tel.RecordDriverSuccess("small")
	srv := New(&Deps{Tel: tel, HistoryMeasurementSources: func() telemetry.ForecastOptions { return telemetry.ForecastOptions{} }})
	drivers := map[string]any{"large": map[string]any{"bat_soc": low}, "small": map[string]any{"bat_soc": high}}
	resp := map[string]any{"bat_soc": .3, "drivers": drivers}
	srv.qualifyStatusHistory(resp, "meter")
	if resp["bat_soc"] != .3 {
		t.Error("capacity-weighted state of charge changed", resp)
	}
	tel.DriverHealthMut("small").SetOffline()
	srv.qualifyStatusHistory(resp, "meter")
	if resp["bat_soc"] != nil || drivers["small"].(map[string]any)["bat_soc"] != nil {
		t.Fatal("offline battery keeps stale state of charge", resp)
	}
}

func TestLiveHistoryQualityConfiguredPVWithoutMeasurementIsUnknown(t *testing.T) {
	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("meter")
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	tel.RecordDriverSuccess("meter")
	srv := New(&Deps{Tel: tel, HistoryMeasurementSources: func() telemetry.ForecastOptions {
		return telemetry.ForecastOptions{HouseholdInvalidReason: "configured_pv_without_measurement_source"}
	}})
	resp := map[string]any{}
	srv.qualifyStatusHistory(resp, "meter")
	if resp["pv_w"] != nil || resp["load_w"] != float64(1200) || resp["load_quality"] != "estimated" || resp["history_quality"] != "incomplete" {
		t.Fatal("unmeasured configured PV reported as zero, or Home use blanked", resp)
	}
}

// Anders (Discord, v0.154.1): Growatt offline after sunset, Pixii meter and
// battery read. /api/status showed pv_w and load_w null.
func TestStatusAndersNightAndDay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dark     telemetry.Darkness
		pv       any
		quality  string
		loadWant any
	}{
		{"night", telemetry.DarknessNight, float64(0), "measured", float64(980)},
		{"day", telemetry.DarknessUnknown, nil, "estimated", float64(980)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := telemetry.NewStore()
			for _, name := range []string{"pixii", "growatt"} {
				tel.EnsureDriverHealth(name)
			}
			tel.Update("pixii", telemetry.DerMeter, 780, nil, nil)
			tel.Update("pixii", telemetry.DerBattery, -200, nil, nil)
			tel.RecordDriverSuccess("pixii")
			tel.Update("growatt", telemetry.DerPV, -40, nil, nil)
			tel.DriverHealthMut("growatt").SetOffline()
			dark := tc.dark
			srv := New(&Deps{Tel: tel, HistoryMeasurementSources: func() telemetry.ForecastOptions {
				return telemetry.ForecastOptions{PVDark: func(time.Time) telemetry.Darkness { return dark }}
			}})
			resp := map[string]any{"drivers": map[string]any{"growatt": map[string]any{}}}
			srv.qualifyStatusHistory(resp, "pixii")
			if resp["grid_w"] != float64(780) || resp["pv_w"] != tc.pv || resp["load_w"] != tc.loadWant || resp["load_quality"] != tc.quality {
				t.Fatal(resp)
			}
			// History quality stays separate from live Home use.
			if resp["history_quality"] != "incomplete" || resp["history_reason"] != "offline:growatt:pv" {
				t.Fatal("live display changed history quality", resp)
			}
			if tc.dark == telemetry.DarknessNight && resp["drivers"].(map[string]any)["growatt"].(map[string]any)["pv_w"] == nil {
				t.Fatal("dark inverter not shown as 0 W")
			}
		})
	}
}
