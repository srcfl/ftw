package state

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestHouseholdSubtotalKeepsAcceptedIntervalsAcrossSourceAndWriterGaps(t *testing.T) {
	for _, kind := range []string{"PV missing", "EV missing", "writer gap", "topology", "invalid configuration", "long writer gap"} {
		t.Run(kind, func(t *testing.T) {
			s := sourceStore(t)
			base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
			for i := int64(0); i <= 90; i++ {
				if kind == "writer gap" && i == 42 || kind == "long writer gap" && i >= 42 && i <= 55 {
					continue
				}
				v := HistorySources{GridW: 1200, EVW: 200}
				if kind == "topology" && i >= 42 {
					v.Topology = "replacement meter"
				}
				if i == 42 && kind == "PV missing" {
					v.Missing = MissingPV
				}
				if i == 42 && kind == "EV missing" {
					v.Missing = MissingEV
				}
				if kind == "invalid configuration" && i >= 42 && i <= 54 {
					v.Missing = MissingLoad
				}
				sourceTick(t, s, base+i*10000, v)
			}
			before, err := s.DailyEnergy(base, base+900000)
			incomplete := kind == "invalid configuration" || kind == "long writer gap"
			if err != nil || (before.Missing&MissingLoad != 0) != incomplete {
				t.Fatal("unexpected completeness under the current source-hold policy", before, err)
			}
			check := func(s *Store) {
				t.Helper()
				got, err := s.HouseholdEnergySubtotal(context.Background(), base, base+900000)
				if err != nil || got == nil {
					t.Fatal("accepted intervals hidden", got, err)
				}
				wantWh, wantCoverage := 250.0, int64(900000)
				if incomplete {
					wantWh, wantCoverage = 1000.0/6, 600000
				}
				near(t, "accepted Wh", got.Wh, wantWh)
				if got.CoverageMS != wantCoverage || got.PeriodMS != 900000 || got.Quality != "estimated" {
					t.Fatal("gap became full coverage or measured energy", got)
				}
				after, err := s.DailyEnergy(base, base+900000)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("subtotal rewrote full total or original quality", before, after, err)
				}
			}
			check(s)
			path := s.mainDBPath
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			check(reopened)
		})
	}
}

func TestHouseholdSubtotalCombinesLegacyAndSourceWithoutDoubleCounting(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	for i := int64(0); i <= 30; i++ {
		at := base + i*10000
		p := &HistoryPoint{TsMs: at, GridW: 1000, LoadW: 1000, JSON: `{}`}
		if i == 30 {
			p.Sources = &HistorySources{AtMS: at, Topology: "grid+charger", GridW: 3000, EVW: 2000}
		}
		dashboardTick(t, s, fmt.Sprint(at), p)
	}
	sourceSeries(t, s, base+300000, []float64{1000, 1000, 1000, 1000, 1000})
	got, err := s.HouseholdEnergySubtotal(context.Background(), base, base+600000)
	if err != nil || got == nil || got.CoverageMS != 600000 {
		t.Fatal(got, err)
	}
	near(t, "mixed subtotal", got.Wh, 1000.0/6)
}

func TestHouseholdSubtotalRetainsCoarseEvidenceWithoutSplittingItsBounds(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	minutes := make([]float64, 15)
	for i := range minutes {
		minutes[i] = 1000
	}
	sourceSeries(t, s, base, minutes)
	if err := s.rollSiteEnergy(context.Background(), base+900000); err != nil {
		t.Fatal(err)
	}
	got, err := s.HouseholdEnergySubtotal(context.Background(), base, base+900000)
	if err != nil || got == nil || got.CoverageMS != 900000 {
		t.Fatal(got, err)
	}
	near(t, "retained subtotal", got.Wh, 250)
	if got, err := s.HouseholdEnergySubtotal(context.Background(), base, base+600000); err != nil || got != nil {
		t.Fatal("coarse interval was split into invented observations", got, err)
	}
}

func TestHouseholdSubtotalPreservesSignedBalanceAndDistinguishesZeroFromUnknown(t *testing.T) {
	for _, tc := range []struct {
		name         string
		minutes      []float64
		wantWh       float64
		wantCoverage int64
	}{
		{"signed balance", []float64{-1000, 3000, 1000, 1000, 1000}, 1000.0 / 12, 300000},
		{"zero", []float64{0, 0, 0, 0, 0}, 0, 300000},
		{"negative interval", []float64{-1000, -1000, -1000, -1000, -1000}, 0, 0},
		{"partial tail", []float64{1000, 1000, 1000, 1000}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sourceStore(t)
			base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
			sourceSeries(t, s, base, tc.minutes)
			got, err := s.HouseholdEnergySubtotal(context.Background(), base, base+int64(len(tc.minutes))*60000)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantCoverage == 0 {
				if got != nil {
					t.Fatal("unknown became a known zero", got)
				}
				return
			}
			if got == nil || got.CoverageMS != tc.wantCoverage {
				t.Fatal(got)
			}
			near(t, "signed subtotal", got.Wh, tc.wantWh)
		})
	}
}

func TestHouseholdSubtotalNeverBridgesRestartOrClaimsAbsentEvidence(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	if got, err := s.HouseholdEnergySubtotal(context.Background(), base, base+900000); err != nil || got != nil {
		t.Fatal("empty history became zero", got, err)
	}
	sourceSeries(t, s, base, []float64{1000, 1000, 1000, 1000, 1000})
	path := s.mainDBPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	sourceSeries(t, reopened, base+600000, []float64{1000, 1000, 1000, 1000, 1000})
	got, err := reopened.HouseholdEnergySubtotal(context.Background(), base, base+900000)
	if err != nil || got == nil || got.CoverageMS != 600000 {
		t.Fatal(got, err)
	}
	near(t, "restart subtotal", got.Wh, 1000.0/6)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reopened.HouseholdEnergySubtotal(ctx, base, base+900000); err == nil {
		t.Fatal("cancelled diagnostic read succeeded")
	}
}
