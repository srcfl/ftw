package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Status sends one Home use: the live balance and its quality. A second,
// energy-averaged number made every client show the live value as an estimate.
func TestStatusSendsOneHomeUse(t *testing.T) {
	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("site")
	base := time.Now().Add(-20 * time.Second)
	for i := 0; i <= 10; i++ {
		data, _ := json.Marshal(map[string]any{"power_observed_at": base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)})
		tel.Update("site", telemetry.DerMeter, 1000+float64(i)*100, nil, data)
	}
	srv := New(&Deps{Tel: tel, HistoryMeasurementSources: func() telemetry.ForecastOptions { return telemetry.ForecastOptions{} }})
	resp := map[string]any{}
	for i := 0; i < 3; i++ {
		srv.qualifyStatusHistory(resp, "site")
		if _, ok := resp["load_estimate"]; ok {
			t.Fatal("status sent a second Home use")
		}
		if resp["load_w"] != float64(2000) || resp["load_quality"] != telemetry.LoadMeasured || resp["grid_w"] != float64(2000) || resp["history_quality"] != "complete" {
			t.Fatal(resp)
		}
	}
	tel.DriverHealthMut("site").SetOffline()
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != nil || resp["load_quality"] != telemetry.LoadUnknown {
		t.Fatal(resp)
	}
}

func TestStatusKeepsPowerBalanceWhenSessionEnergyCannotFormAnInterval(t *testing.T) {
	tel := telemetry.NewStore()
	for _, name := range []string{"site", "charger"} {
		tel.EnsureDriverHealth(name)
	}
	old := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	data, err := json.Marshal(map[string]any{
		"connected": true, "charging": false,
		"control_power_observed_at": old, "control_power_confirmed": true,
		"control_power_available": true,
		"session_wh":              5000, "energy_observed_at": old,
	})
	if err != nil {
		t.Fatal(err)
	}
	tel.Update("site", telemetry.DerMeter, 1234, nil, nil)
	tel.Update("charger", telemetry.DerEV, 0, nil, data)
	srv := New(&Deps{Tel: tel, HistoryMeasurementSources: func() telemetry.ForecastOptions { return telemetry.ForecastOptions{} }})
	resp := map[string]any{}
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != float64(1234) || resp["history_quality"] != "complete" {
		t.Fatal("valid power balance disappeared")
	}
	tel.DriverHealthMut("charger").SetOffline()
	srv.qualifyStatusHistory(resp, "site")
	// The offline charger is left out of live Home use, which says so.
	if resp["load_w"] != float64(1234) || resp["load_quality"] != "estimated" {
		t.Fatal("offline charger blanked Home use")
	}
	tel.RecordDriverSuccess("charger")
	tel.Update("charger", telemetry.DerEV, 6000, nil, nil)
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != float64(1234-6000) || resp["history_reason"] != "invalid_balance" {
		t.Fatal("negative residual was hidden or zeroed")
	}
}

func TestStatusHomePowerUsesPixiiACWithoutChangingRawHistoryInputs(t *testing.T) {
	tel := telemetry.NewStore()
	for _, name := range []string{"site", "battery", "charger"} {
		tel.EnsureDriverHealth(name)
	}
	tel.Update("site", telemetry.DerMeter, 5000, nil, nil)
	tel.Update("battery", telemetry.DerBattery, -2500, nil, json.RawMessage(`{"control_power_w":-2000,"control_power_available":true}`))
	tel.Update("charger", telemetry.DerEV, 6000, nil, nil)
	srv := New(&Deps{Tel: tel, HistoryMeasurementSources: func() telemetry.ForecastOptions { return telemetry.ForecastOptions{} }})
	resp := map[string]any{}
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != float64(1000) || resp["bat_w"] != float64(-2000) {
		t.Fatal("household AC balance used battery DC power")
	}
	raw := tel.ForecastMeasurementNow("site", telemetry.ForecastOptions{})
	if raw.BatteryW != -2500 || raw.HouseholdW != 1500 {
		t.Fatal("display changed raw forecast/history input")
	}
	tel.Update("battery", telemetry.DerBattery, -2500, nil, json.RawMessage(`{"control_power_available":false}`))
	srv.qualifyStatusHistory(resp, "site")
	// Unknown AC power leaves the battery out; the balance is shown as is.
	if resp["load_w"] != float64(5000-6000) || resp["bat_w"] != nil || resp["load_quality"] != "estimated" {
		t.Fatal("missing AC measurement became DC power or hid the balance")
	}
	tel.Update("battery", telemetry.DerBattery, -2500, nil, json.RawMessage(`{"control_power_w":-500,"control_power_available":true}`))
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != float64(5000-(-500)-6000) || resp["history_reason"] != "invalid_balance" {
		t.Fatal("negative AC balance became zero or a DC estimate")
	}
}

func TestEnergyTodayKeepsEstimatedLoadOutOfMeasuredFields(t *testing.T) {
	got := energyTodayFields(state.DayEnergy{Intervals: 2, Missing: state.EstimatedLoad, LoadWh: 22, ImportWh: 10})
	if got["history_quality"] != "estimated" || got["load_wh"] != float64(22) {
		t.Fatal(got)
	}
	measured := got["measured"].(map[string]any)
	if measured["load_wh"] != nil || measured["import_wh"] != float64(10) {
		t.Fatal(measured)
	}
}

func TestDailyEnergyCacheWaitsForMidnightRecovery(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	// Include the 23/25-hour days: a local day is not always 24 hours.
	for _, day := range []time.Time{time.Date(2026, 10, 17, 0, 0, 0, 0, loc), time.Date(2026, 10, 25, 0, 0, 0, 0, loc), time.Date(2026, 3, 29, 0, 0, 0, 0, loc)} {
		end := day.AddDate(0, 0, 1)
		for _, elapsed := range []time.Duration{-time.Second, 0, 30 * time.Second, 59 * time.Second, 2 * time.Minute, 179 * time.Second} {
			if dailyEnergyCacheReady(day, end.Add(elapsed)) {
				t.Fatal("cached before recovery can finish", day, elapsed)
			}
		}
		if !dailyEnergyCacheReady(day, end.Add(3*time.Minute)) {
			t.Fatal("closed day never became cacheable", day)
		}
	}
}
