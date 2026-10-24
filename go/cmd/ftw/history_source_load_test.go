package main

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"

	"github.com/srcfl/ftw/go/internal/ocpp"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestHistoryLoadIndependentReportingCadence(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	tel.Update("grid", telemetry.DerMeter, 4000, nil, nil)
	tel.RecordDriverSuccess("grid")
	tel.Update("charger", telemetry.DerEV, 3000, nil, nil)
	tel.RecordDriverSuccess("charger")
	// Both powers are fresh. The charger reports more slowly than the meter.
	tel.Get("charger", telemetry.DerEV).UpdatedAt = time.Now().Add(-45 * time.Second)
	base := time.Now().Add(-10 * time.Minute).Truncate(5 * time.Minute).UnixMilli()
	for i := int64(0); i <= 30; i++ {
		p, _ := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "grid"}, base+i*10000, 90*time.Second)
		if p.Sources.Missing != 0 {
			t.Fatal("fresh charger rejected by historical source qualification")
		}
		p.Sources.AtMS = base + i*10000
		if err := st.RecordTickWithEnergy(p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, err := st.DailyEnergy(base, base+300000)
	if err != nil {
		t.Fatal(err)
	}
	if d.Missing&state.MissingLoad != 0 || math.Abs(d.LoadWh-1000.0/12) > 0.001 {
		t.Fatal("fresh asynchronous sources lost historical household energy")
	}
	native, err := (&appHistory{st: st}).Window(context.Background(), []string{"load_w"}, 300000, base, base+300000)
	if err != nil {
		t.Fatal(err)
	}
	if len(native["load_w"]) != 1 || math.Abs(native["load_w"][0].W-1000) > 0.001 {
		t.Fatal("native history did not receive the interval household balance")
	}
}

// A silent inverter is unknown on its own. Its balance with the meter is
// held, not voided, so the household keeps its estimated energy.
func TestHistoryHoldsASilentSourceThroughTheBalance(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	tel.Update("grid", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("grid")
	tel.Update("sma", telemetry.DerPV, 0, nil, nil)
	tel.RecordDriverSuccess("sma")
	base := time.Now().Add(-20 * time.Minute).Truncate(5 * time.Minute).UnixMilli()
	for i := int64(0); i <= 60; i++ {
		if i == 6 {
			tel.Get("sma", telemetry.DerPV).UpdatedAt = time.Now().Add(-10 * time.Minute)
		}
		p, _ := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "grid"}, base+i*10000, 90*time.Second)
		if i >= 6 && p.Sources.Missing != state.MissingPV {
			t.Fatalf("silent inverter marked %b, want only its own flow", p.Sources.Missing)
		}
		p.Sources.AtMS = base + i*10000
		if err := st.RecordTickWithEnergy(p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, err := st.DailyEnergy(base, base+600000)
	if err != nil {
		t.Fatal(err)
	}
	if d.Missing&state.MissingLoad != 0 || math.Abs(d.LoadWh-1000.0/6) > 0.001 {
		t.Fatalf("household lost through a silent inverter: %v Wh, quality %s", d.LoadWh, d.Missing.Quality())
	}
}

func TestHistorySourceQualificationDoesNotInventMissingFlows(t *testing.T) {
	for _, tc := range []struct {
		reason string
		watts  float64
		known  bool
		want   state.HistoryMissing
	}{
		{"missing_config", 0, true, state.MissingLoad},
		{"duplicate_charger:easee", 0, true, state.MissingLoad},
		{"invalid_balance", 1000, true, 0},
		{"nonfinite_sum", math.Inf(1), false, state.MissingLoad | state.MissingEV},
		{"stale:easee:ev", 0, false, state.MissingEV},
		{"offline:easee:ev", 0, false, state.MissingEV},
	} {
		known := map[telemetry.DerType]bool{telemetry.DerMeter: true, telemetry.DerPV: true, telemetry.DerBattery: true, telemetry.DerEV: tc.known, telemetry.DerV2X: true}
		p := historySources(telemetry.ForecastReading{At: time.Now(), Known: known, Reason: tc.reason, EVW: tc.watts})
		if p.Missing != tc.want || math.IsNaN(p.EVW) || math.IsInf(p.EVW, 0) {
			t.Fatal("history source qualification lost missing evidence or rejected unrelated telemetry")
		}
	}
}

// An OCPP charger with no car sends no MeterValues. Its Available status is
// the evidence that nothing flows, so the household stays measured.
func TestHistoryCountsAnEmptyOCPPChargerAsMeasured(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	h := ocpp.NewHandler(tel, 60)
	h.SetApprovedIDs([]string{"charger"})
	h.OnConnect("charger")
	if _, err := h.OnStatusNotification("charger", &core.StatusNotificationRequest{ConnectorId: 1, Status: core.ChargePointStatusAvailable, ErrorCode: core.NoError}); err != nil {
		t.Fatal(err)
	}
	tel.Update("grid", telemetry.DerMeter, 2000, nil, nil)
	tel.RecordDriverSuccess("grid")
	if live := tel.LiveMeasurementNow("grid", telemetry.ForecastOptions{}); live.LoadQuality != telemetry.LoadMeasured || live.LoadW != 2000 {
		t.Fatalf("live Home use %s (%s), %v W", live.LoadQuality, live.LoadReason, live.LoadW)
	}
	base := time.Now().Add(-10 * time.Minute).Truncate(5 * time.Minute).UnixMilli()
	for i := int64(0); i <= 30; i++ {
		p, _ := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "grid"}, base+i*10000, time.Minute)
		if p.Sources.Missing != 0 || p.Missing&^state.MissingSoC != 0 {
			t.Fatalf("empty charger marked history %b, sources %b", p.Missing, p.Sources.Missing)
		}
		p.Sources.AtMS = base + i*10000
		if err := st.RecordTickWithEnergy(p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, err := st.DailyEnergy(base, base+300000)
	if err != nil {
		t.Fatal(err)
	}
	if d.Missing&state.MissingLoad != 0 || math.Abs(d.LoadWh-2000.0/12) > 0.001 {
		t.Fatalf("household %v Wh, quality %s", d.LoadWh, d.Missing.Quality())
	}
}

// Between heartbeats an Available charger's 0 W is up to a minute old. The
// time-aligned balance calls that time_skew; the chart's quality comes from
// the source rows, so it must not say the measurements do not balance.
func TestHistoryChartKeepsSourceQualityForAnAgeingOCPPZero(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	tel.Update("grid", telemetry.DerMeter, 2000, nil, nil)
	tel.RecordDriverSuccess("grid")
	// What the OCPP handler publishes 40 s after the last heartbeat.
	at := time.Now().Add(-40 * time.Second).UnixMilli()
	data, _ := json.Marshal(map[string]any{"type": "ev", "w": 0.0, "connected": false, "forecast_power": telemetry.ForecastPowerSample{Version: 1, Known: true, MeasuredAtMS: at, ReceivedAtMS: at}})
	tel.Update("charger", telemetry.DerEV, 0, nil, data)
	tel.RecordDriverSuccess("charger")
	base := time.Now().Add(-10 * time.Minute).Truncate(5 * time.Minute).UnixMilli()
	for i := int64(0); i <= 30; i++ {
		p, _ := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "grid"}, base+i*10000, time.Minute)
		if p.Missing&state.BalanceMismatch == 0 || p.Sources.Missing != 0 {
			t.Fatalf("want a skewed aligned balance and complete sources, got %b and %b", p.Missing, p.Sources.Missing)
		}
		p.Sources.AtMS = base + i*10000
		if err := st.RecordTickWithEnergy(p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	points, err := st.LoadHistoryContext(context.Background(), base, base+300000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) == 0 {
		t.Fatal("no chart points")
	}
	for _, p := range points {
		if q := p.Missing.Quality(); q == "balance_mismatch" || q == "incomplete" {
			t.Fatalf("chart point at %d says %s", p.TsMs, q)
		}
	}
}

// An idle charger often sends only heartbeats. At Core's default interval,
// with ±1 s jitter, its Available 0 W is at most 31 s old at any 2 s history
// tick: inside the 60 s limit, and its health never lapses. The old 60 s
// default loses ticks to the same jitter.
func TestHistoryKeepsAnIdleOCPPChargerMeasuredBetweenHeartbeats(t *testing.T) {
	var c ocpp.Config
	c.Defaults()
	if lost := idleOCPPStaleTicks(t, time.Duration(c.HeartbeatIntervalS)*time.Second, true); lost != 0 {
		t.Fatalf("default heartbeat %d s: %d ticks lost the charger", c.HeartbeatIntervalS, lost)
	}
	if lost := idleOCPPStaleTicks(t, time.Minute, false); lost == 0 {
		t.Fatal("a 60 s heartbeat lost no tick; the test cannot see the edge")
	}
}

// idleOCPPStaleTicks plays ten heartbeats at interval ±1 s through the real
// handler and counts history ticks whose source row misses a flow. Between
// beats the charger's reading and health are aged by the time since the beat.
func idleOCPPStaleTicks(t *testing.T, interval time.Duration, record bool) int {
	t.Helper()
	const watchdog = time.Minute // site default; history uses it as its age limit
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	h := ocpp.NewHandler(tel, int(interval/time.Second))
	h.SetApprovedIDs([]string{"charger"})
	h.OnConnect("charger")
	if _, err := h.OnStatusNotification("charger", &core.StatusNotificationRequest{ConnectorId: 1, Status: core.ChargePointStatusAvailable, ErrorCode: core.NoError}); err != nil {
		t.Fatal(err)
	}
	age := func(beat time.Time, since time.Duration) {
		r := tel.Get("charger", telemetry.DerEV)
		var d map[string]any
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		at := beat.Add(-since)
		d["forecast_power"] = telemetry.ForecastPowerSample{Version: 1, Known: true, MeasuredAtMS: at.UnixMilli(), ReceivedAtMS: at.UnixMilli()}
		r.Data, _ = json.Marshal(d)
		r.UpdatedAt = at
		tel.DriverHealthMut("charger").LastSuccess = &at
	}
	base := time.Now().Add(-time.Hour).Truncate(5 * time.Minute).UnixMilli()
	tick, lost := base, 0
	for beat := 0; beat < 10; beat++ {
		gap := interval + []time.Duration{-time.Second, 0, time.Second}[beat%3]
		if _, err := h.OnHeartbeat("charger", &core.HeartbeatRequest{}); err != nil {
			t.Fatal(err)
		}
		beatAt := time.Now()
		for since := time.Duration(beat%4) * 500 * time.Millisecond; since < gap; since += 2 * time.Second {
			age(beatAt, since)
			tel.Update("grid", telemetry.DerMeter, 2000, nil, nil)
			tel.RecordDriverSuccess("grid")
			tel.WatchdogScan(watchdog)
			p, _ := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "grid"}, tick, watchdog)
			if p.Sources.Missing != 0 {
				lost++
			}
			if record {
				p.Sources.AtMS = tick
				if err := st.RecordTickWithEnergy(p, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			tick += 2000
		}
	}
	if record {
		if err := st.FlushHistory(context.Background()); err != nil {
			t.Fatal(err)
		}
		d, err := st.DailyEnergy(base, tick)
		if err != nil {
			t.Fatal(err)
		}
		if d.Missing&(state.MissingLoad|state.MissingEV) != 0 || d.LoadWh <= 0 {
			t.Fatalf("history over %d s: %v Wh, quality %s", (tick-base)/1000, d.LoadWh, d.Missing.Quality())
		}
	}
	return lost
}
