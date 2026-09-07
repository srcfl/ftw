package telemetry

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func forecastStore(now time.Time, grid, pv, bat float64) *Store {
	s := NewStore()
	for _, v := range []struct {
		d string
		k DerType
		w float64
	}{{"site", DerMeter, grid}, {"pv", DerPV, pv}, {"bat", DerBattery, bat}} {
		s.Update(v.d, v.k, v.w, nil, nil)
		s.RecordDriverSuccess(v.d)
		s.readings[v.d+":"+v.k.String()].UpdatedAt = now
	}
	return s
}
func TestForecastCompleteBalance(t *testing.T) {
	now := time.Now()
	s := forecastStore(now, 4000, 0, 3000)
	if r := s.ForecastMeasurement(now, "site", ForecastOptions{}); !r.Valid || r.HouseholdW != 1000 {
		t.Fatalf("balance: %+v", r)
	}
	s.SetDriverCommandFault("bat", true, "refused")
	if r := s.ForecastMeasurement(now, "site", ForecastOptions{}); !r.Valid || r.HouseholdW != 1000 {
		t.Fatalf("fresh command fault: %+v", r)
	}
	s.health["bat"].SetOffline()
	if r := s.ForecastMeasurement(now, "site", ForecastOptions{}); r.Valid || !r.PVValid {
		t.Fatalf("missing battery should only invalidate household: %+v", r)
	}
}
func TestForecastRejectsIncompleteAndSkewedFlows(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		change func(*Store)
		opts   ForecastOptions
	}{
		{"stale PV", func(s *Store) { s.readings["pv:pv"].UpdatedAt = now.Add(-2 * time.Minute) }, ForecastOptions{}},
		{"skew", func(s *Store) { s.readings["pv:pv"].UpdatedAt = now.Add(-40 * time.Second) }, ForecastOptions{}},
		{"nonfinite", func(s *Store) { s.readings["bat:battery"].RawW = math.NaN() }, ForecastOptions{}},
		{"future", func(s *Store) { s.readings["bat:battery"].UpdatedAt = now.Add(time.Second) }, ForecastOptions{}},
		{"never emitted", func(s *Store) {}, ForecastOptions{ExpectedFlows: []ForecastFlow{{Driver: "charger", DerType: DerEV}}}},
		{"duplicate", func(s *Store) {}, ForecastOptions{ExpectedFlows: []ForecastFlow{{Driver: "pv", DerType: DerPV, FlowID: "roof"}, {Driver: "second", DerType: DerPV, FlowID: "roof"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := forecastStore(now, 4000, -2000, 3000)
			tc.change(s)
			r := s.ForecastMeasurement(now, "site", tc.opts)
			if r.Valid || r.Reason == "" {
				t.Fatalf("accepted %+v", r)
			}
		})
	}
}
func TestForecastGrossHouseCanExceedGridFuse(t *testing.T) {
	now := time.Now()
	s := forecastStore(now, 8000, -7000, 0)
	r := s.ForecastMeasurement(now, "site", ForecastOptions{})
	if !r.Valid || r.HouseholdW != 15000 {
		t.Fatalf("valid 15kW house with 8kW grid: %+v", r)
	}
}
func TestForecastIntervalsCadenceAndGaps(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, cadence := range []time.Duration{time.Second, 10 * time.Second, time.Minute} {
		a := ForecastAccumulator{}
		var intervals []ForecastInterval
		for elapsed := time.Duration(0); elapsed <= 30*time.Minute; elapsed += cadence {
			at := start.Add(elapsed)
			r := ForecastReading{At: at, Latest: at, Valid: true, PVValid: true, HouseholdW: 1000 + elapsed.Seconds(), PVW: -2000}
			intervals = append(intervals, a.Observe(r)...)
		}
		if len(intervals) != 2 || math.Abs(intervals[0].HouseholdW-1450) > 0.001 || !intervals[0].PVValid {
			t.Fatalf("cadence %s: %+v", cadence, intervals)
		}
	}
	a := ForecastAccumulator{}
	for i := 0; i <= 15; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		r := ForecastReading{At: at, Latest: at, Valid: i != 5, PVValid: true, HouseholdW: 1000}
		if out := a.Observe(r); len(out) > 0 {
			t.Fatalf("gap produced complete interval: %+v", out)
		}
	}
}
func TestForecastIntervalMissingPVIsNotZeroTruth(t *testing.T) {
	a := ForecastAccumulator{}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var got []ForecastInterval
	for i := 0; i <= 15; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		got = append(got, a.Observe(ForecastReading{At: at, Latest: at, Valid: true, PVValid: false, HouseholdW: 1000})...)
	}
	if len(got) != 1 || got[0].PVValid {
		t.Fatalf("PV absence lost: %+v", got)
	}
}

func TestForecastDuplicateOrFutureSamplesBreakInterval(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, kind := range []string{"duplicate", "future"} {
		t.Run(kind, func(t *testing.T) {
			a := ForecastAccumulator{}
			for i := 0; i <= 15; i++ {
				at := start.Add(time.Duration(i) * time.Minute)
				latest := at
				if i == 7 {
					if kind == "duplicate" {
						latest = at.Add(-time.Minute)
					} else {
						latest = at.Add(time.Minute)
					}
				}
				if out := a.Observe(ForecastReading{At: at, Latest: latest, Valid: true, PVValid: true, HouseholdW: 1000}); len(out) > 0 {
					t.Fatalf("%s created a complete interval: %+v", kind, out)
				}
			}
		})
	}
}
func BenchmarkForecastMeasurement(b *testing.B) {
	now := time.Now()
	s := forecastStore(now, 4000, -2000, 3000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.ForecastMeasurement(now, "site", ForecastOptions{})
	}
}

func TestForecastIndependentPVInvalidReason(t *testing.T) {
	now := time.Now()
	s := forecastStore(now, 2000, -1000, 0)
	r := s.ForecastMeasurement(now, "site", ForecastOptions{PVInvalidReason: "unconfirmed_pv_identity"})
	if !r.Valid || r.PVValid || r.PVReason != "unconfirmed_pv_identity" {
		t.Fatalf("PV qualification damaged independent balance: %+v", r)
	}
	r = s.ForecastMeasurement(now, "site", ForecastOptions{HouseholdInvalidReason: "unconfirmed_ev_identity"})
	if r.Valid || !r.PVValid {
		t.Fatalf("household identity blocked independent PV: %+v", r)
	}
}

func TestForecastPowerMetadataUsesMeasurementNotStatus(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	good := ForecastPowerSample{Version: 1, Known: true, Watts: 700, MeasuredAtMS: now.Add(-time.Second).UnixMilli(), ReceivedAtMS: now.UnixMilli()}
	for name, change := range map[string]func(*ForecastPowerSample){"real zero": func(p *ForecastPowerSample) { p.Watts = 0 }, "old": func(p *ForecastPowerSample) { p.MeasuredAtMS = now.Add(-2 * time.Minute).UnixMilli() }, "unknown": func(p *ForecastPowerSample) { p.Known = false }, "future receive": func(p *ForecastPowerSample) { p.ReceivedAtMS = now.Add(time.Second).UnixMilli() }, "future sample": func(p *ForecastPowerSample) { p.MeasuredAtMS = now.Add(time.Second).UnixMilli() }, "wrong version": func(p *ForecastPowerSample) { p.Version = 2 }} {
		t.Run(name, func(t *testing.T) {
			s := forecastStore(now, 5000, -1000, 0)
			sample := good
			change(&sample)
			data, _ := json.Marshal(map[string]any{"forecast_power": sample})
			s.Update("charger", DerEV, 9999, nil, data)
			s.RecordDriverSuccess("charger")
			r := s.ForecastMeasurement(time.Now(), "site", ForecastOptions{})
			if name == "real zero" {
				if !r.Valid || r.EVW != 0 || r.HouseholdW != 6000 {
					t.Fatalf("measured zero lost to status rawW: %+v", r)
				}
			} else if r.Valid || !r.PVValid {
				t.Fatalf("invalid power time/quality accepted or affected PV: %+v", r)
			}
		})
	}
}
