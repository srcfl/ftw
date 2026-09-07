package forecast

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestInvalidDirectPVPreservesIndependentWeather(t *testing.T) {
	for _, invalid := range []float64{-1, math.NaN(), math.Inf(1)} {
		st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		temp := 10.0
		at := time.Now().UTC().Truncate(time.Hour)
		s := &Service{Store: st, RatedPVW: 10000, Provider: staticForecastProvider{rows: []RawForecast{{HourStart: at, PVWEstimated: &invalid, TempC: &temp}}}}
		if !s.fetchAndStore(context.Background()) {
			t.Fatal("valid temperature row was discarded")
		}
		rows, err := st.LoadForecasts(at.UnixMilli(), at.Add(time.Hour).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].PVWEstimated != nil || rows[0].TempC == nil || *rows[0].TempC != 10 {
			t.Fatalf("invalid PV became a measured zero or lost temperature: %+v", rows)
		}
		_ = st.Close()
	}
}

func TestUnknownPVScaleAndDCNameplateAreNotACLimits(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().UTC().Truncate(time.Hour)
	ghi := 1200.0
	s := &Service{Store: st, Provider: staticForecastProvider{rows: []RawForecast{{HourStart: at, SolarWm2: &ghi}}}}
	s.fetchAndStore(context.Background())
	rows, _ := s.Load(at.UnixMilli(), at.Add(time.Hour).UnixMilli())
	if len(rows) != 1 || rows[0].PVWEstimated != nil {
		t.Fatalf("unknown scale became a zero forecast: %+v", rows)
	}
	s.RatedPVW = 10000
	s.fetchAndStore(context.Background())
	rows, _ = s.Load(at.UnixMilli(), at.Add(time.Hour).UnixMilli())
	if rows[0].PVWEstimated == nil || math.Abs(*rows[0].PVWEstimated-12000) > 1e-6 {
		t.Fatal("DC prior imposed an AC cap")
	}
	s.ACLimitW = 9000
	s.fetchAndStore(context.Background())
	rows, _ = s.Load(at.UnixMilli(), at.Add(time.Hour).UnixMilli())
	if rows[0].PVWEstimated == nil || *rows[0].PVWEstimated != 9000 {
		t.Fatal("verified AC cap was ignored")
	}
}
