package pvperf

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/strang"
)

// f64 addresses a literal: config.PVArray keeps tilt and azimuth as pointers so
// an omitted field cannot pass for a valid 0°.
func f64(v float64) *float64 { return &v }

func openStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestFromConfigGating(t *testing.T) {
	st := openStore(t)

	if FromConfig(nil, 5000, st, "ua") != nil {
		t.Error("nil weather config should yield nil service")
	}
	if FromConfig(&config.Weather{Latitude: 59, Longitude: 18}, 0, st, "ua") != nil {
		t.Error("no arrays and no rated PV should yield nil service")
	}

	// Explicit arrays win.
	svc := FromConfig(&config.Weather{
		Latitude: 59, Longitude: 18,
		PVArrays: []config.PVArray{
			{RatedW: 5000, TiltDeg: f64(35), AzimuthDeg: f64(180)},
			{RatedW: 3000, TiltDeg: f64(20), AzimuthDeg: f64(90)},
		},
	}, 0, st, "ua")
	if svc == nil || len(svc.Arrays) != 2 {
		t.Fatalf("expected 2 arrays, got %+v", svc)
	}
	if svc.Arrays[0].RatedW != 5000 || svc.Arrays[1].AzimuthDeg != 90 {
		t.Errorf("array geometry mismatch: %+v", svc.Arrays)
	}

	// An array whose tilt was never filled in is not a 0° flat roof; scoring
	// against it would invent a plane the operator never described, so the
	// incomplete entry is skipped and only the usable one survives.
	partial := FromConfig(&config.Weather{
		Latitude: 59, Longitude: 18,
		PVArrays: []config.PVArray{
			{RatedW: 5000, AzimuthDeg: f64(180)},
			{RatedW: 3000, TiltDeg: f64(20), AzimuthDeg: f64(90)},
		},
	}, 0, st, "ua")
	if partial == nil || len(partial.Arrays) != 1 {
		t.Fatalf("incomplete geometry should be skipped, got %+v", partial)
	}
	if partial.Arrays[0].RatedW != 3000 {
		t.Errorf("wrong array survived: %+v", partial.Arrays[0])
	}

	// Flat fallback synthesizes one array from rated + legacy tilt/azimuth.
	flat := FromConfig(&config.Weather{
		Latitude: 59, Longitude: 18, PVTiltDeg: 30, PVAzimuthDeg: 180,
	}, 8000, st, "ua")
	if flat == nil || len(flat.Arrays) != 1 {
		t.Fatalf("flat fallback should synthesize one array, got %+v", flat)
	}
	if flat.Arrays[0].RatedW != 8000 || flat.Arrays[0].TiltDeg != 30 {
		t.Errorf("synthesized array mismatch: %+v", flat.Arrays[0])
	}
}

// A bell-ish clear day of hourly GHI centered on solar noon.
func syntheticDay(dayStart time.Time) []strang.IrradianceHour {
	out := []strang.IrradianceHour{}
	for hr := 4; hr <= 20; hr++ {
		// crude parabola peaking ~700 W/m² at hour 12
		g := 700.0 - 12.0*float64((hr-12)*(hr-12))
		if g < 0 {
			g = 0
		}
		out = append(out, strang.IrradianceHour{
			HourStart: dayStart.Add(time.Duration(hr) * time.Hour),
			GHIWm2:    g,
		})
	}
	return out
}

func TestScoreDayPersistsExpectedVsActual(t *testing.T) {
	st := openStore(t)
	svc := &Service{
		Store:  st,
		Lat:    59.33,
		Lon:    18.07,
		Arrays: []Array{{RatedW: 10000, TiltDeg: 35, AzimuthDeg: 180}},
	}

	dayStart := time.Date(2024, 6, 21, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.AddDate(0, 0, 1)
	day := dayStart.Format("2006-01-02")

	// Seed measured PV history: -2000 W constant across 8h → 16000 Wh produced
	// (PV is stored site-signed negative; DailyEnergy integrates SUM(-pv_w·Δt)).
	if err := st.RecordHistory(state.HistoryPoint{TsMs: dayStart.Add(8 * time.Hour).UnixMilli(), PVW: -2000}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHistory(state.HistoryPoint{TsMs: dayStart.Add(16 * time.Hour).UnixMilli(), PVW: -2000}); err != nil {
		t.Fatal(err)
	}

	hours := syntheticDay(dayStart)
	if !svc.scoreDay(day, dayStart, dayEnd, hours, 12345) {
		t.Fatal("scoreDay should succeed with history present")
	}

	got, ok, err := st.LoadPVPerformanceDay(day)
	if err != nil || !ok {
		t.Fatalf("score not persisted: ok=%v err=%v", ok, err)
	}
	if math.Abs(got.ActualWh-16000) > 1 {
		t.Errorf("actual Wh: want ~16000, got %.1f", got.ActualWh)
	}
	if got.ExpectedWh <= 0 {
		t.Errorf("expected Wh should be positive, got %.1f", got.ExpectedWh)
	}
	if got.PR == nil {
		t.Error("PR should be set when expected is above the floor")
	}
	if got.StrangDataDateMs == nil || *got.StrangDataDateMs != 12345 {
		t.Errorf("provenance not stamped: %+v", got.StrangDataDateMs)
	}
}

func TestScoreDaySkipsWhenNoHistory(t *testing.T) {
	st := openStore(t)
	svc := &Service{Store: st, Lat: 59.33, Lon: 18.07, Arrays: []Array{{RatedW: 10000, TiltDeg: 35, AzimuthDeg: 180}}}

	dayStart := time.Date(2024, 6, 21, 0, 0, 0, 0, time.UTC)
	day := dayStart.Format("2006-01-02")
	if svc.scoreDay(day, dayStart, dayStart.AddDate(0, 0, 1), syntheticDay(dayStart), 1) {
		t.Error("scoreDay should return false with no measured history")
	}
	if _, ok, _ := st.LoadPVPerformanceDay(day); ok {
		t.Error("nothing should be persisted when there's no history")
	}
}
