package state

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestSignedBalanceEstimateKeepsNegativeTransientsOutOfTraining(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	// Same real 1 kW house, asynchronously reported battery step: household
	// residual goes -1 kW then +3 kW. Summing signed source energy gives 1 kW.
	for i, w := range []float64{1000, -1000, 3000, 1000} {
		p := HistoryPoint{TsMs: base + int64(i)*10000, GridW: 1000, BatW: 1000 - w, LoadW: w, JSON: `{"ev_w":0,"v2x_w":0,"history_reason":""}`}
		if w < 0 {
			p.Missing = MissingLoad | BalanceMismatch
			p.LoadW = 0
			p.JSON = `{"ev_w":0,"v2x_w":0,"history_reason":"invalid_balance"}`
		}
		dashboardTick(t, s, fmt.Sprint(i), &p)
	}
	check := func(s *Store) {
		t.Helper()
		d, e := s.DailyEnergy(base, base+60000)
		if e != nil {
			t.Fatal(e)
		}
		near(t, "signed house energy", d.LoadWh, 1000*30.0/3600)
		covered, err := s.HistoryEnergyCoverage(context.Background(), base, base+60000)
		if err != nil || covered != 30000 {
			t.Fatal(covered, err)
		}

		if d.Missing != EstimatedLoad || d.Missing.Quality() != "estimated" {
			t.Fatal(d)
		}
		var mask HistoryMissing
		if err := s.history.QueryRow(`SELECT missing FROM history_quality WHERE kind='partial_dashboard' LIMIT 1`).Scan(&mask); err != nil || mask != MissingLoad|BalanceMismatch {
			t.Fatal(mask, err)
		}
	}
	check(s)
	if err := s.maintainDashboard(context.Background(), time.Now().Add(40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(s)
}

func TestResidualEstimateRejectsMissingSkewedAndDuplicateSources(t *testing.T) {
	for _, tc := range []struct {
		mask   HistoryMissing
		reason string
	}{
		{MissingLoad | MissingBattery, "missing:battery"}, {MissingLoad | MissingEV, "missing:ev"},
		{MissingLoad | BalanceMismatch, "time_skew"}, {MissingLoad | BalanceMismatch, "duplicate_flow:battery"},
	} {
		p := HistoryPoint{Missing: tc.mask, JSON: fmt.Sprintf(`{"ev_w":0,"v2x_w":0,"history_reason":%q}`, tc.reason)}
		if _, ok := residualHouseholdW(&p); ok {
			t.Fatal(tc)
		}
	}
	// A physically impossible daily total must remain unknown.
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	for i := 0; i < 4; i++ {
		p := HistoryPoint{TsMs: base + int64(i)*10000, GridW: 1000, BatW: 4000, Missing: MissingLoad | BalanceMismatch, JSON: `{"ev_w":0,"v2x_w":0,"history_reason":"invalid_balance"}`}
		dashboardTick(t, s, fmt.Sprint(i), &p)
	}
	d, err := s.DailyEnergy(base, base+60000)
	if err != nil || d.Missing&MissingLoad == 0 {
		t.Fatal(d, err)
	}
}
