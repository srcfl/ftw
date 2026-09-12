package main

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/mpc"
)

func TestForecastPrimaryLoadQualityKeepsSitePriorUntilLearning(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	for _, quality := range []string{"cold_start", "learning", "ready"} {
		t.Run(quality, func(t *testing.T) {
			f := primaryFixture(at, func(_ context.Context, payload []byte) ([]byte, error) {
				_, reply := hostForecastReply(payload)
				for _, row := range reply["predictions"].([]any) {
					row.(map[string]any)["load"] = map[string]any{"known": true, "point_w": 0, "quality": quality, "uncertainty": "unavailable", "coverage": 0.5}
				}
				return json.Marshal(reply)
			})
			in := f.Snapshot(at, trackerWeather(at, at))
			legacy := trackerSlots(at, 1)
			legacy[0].LoadW = 6604
			got := in.Resolve(context.Background(), legacy)
			want, source := 0.0, "energyplan"
			if quality == "cold_start" {
				want, source = legacy[0].LoadW, "legacy"
			}
			if got[0].LoadW != want || got[0].PVW != -100 {
				t.Fatalf("load selection changed the site prior or independent PV: %+v", got)
			}
			in.Record(got, got, "load-quality", at.UnixMilli())
			issue := (<-f.queue).issue
			point := primarySeries(t, issue, "champion").Points[0]
			if point.LoadW != want || point.LoadSource != source {
				t.Fatalf("archive does not explain selected load: %+v", point)
			}
			if raw := primarySeries(t, issue, "energyplan").Points[0]; raw.LoadW != 0 || raw.LoadQuality != quality {
				t.Fatal("selection rewrote the worker forecast")
			}
		})
	}
}

func TestForecastRiskUsesCalibratedLoadWithoutPVAndJointErrorsOnce(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	origin := start.AddDate(0, 0, 8)
	for _, pvKnown := range []bool{false, true} {
		name := "load_only"
		if pvKnown {
			name = "joint_net"
		}
		t.Run(name, func(t *testing.T) {
			f := primaryFixture(origin, func(_ context.Context, payload []byte) ([]byte, error) {
				_, reply := hostForecastReply(payload)
				for _, row := range reply["predictions"].([]any) {
					row.(map[string]any)["load"] = map[string]any{"known": true, "point_w": 1000, "lower_w": 500, "upper_w": 4000, "quality": "learning", "uncertainty": "provisional", "coverage": 0.5}
					row.(map[string]any)["pv"] = map[string]any{"known": false, "quality": "unknown", "uncertainty": "unavailable", "coverage": 0}
				}
				return json.Marshal(reply)
			})
			for day := 0; day < 7; day++ {
				for hour := 0; hour < 8; hour++ {
					at := start.Add(time.Duration(day*24+hour) * time.Hour)
					end := at.Add(15 * time.Minute)
					issued := at.Add(-2 * time.Hour)
					band := forecasting.Band{LowW: 0, HighW: 5000, Method: forecasting.BandMethodColdStart}
					p := forecasting.Point{StartMS: at.UnixMilli(), EndMS: end.UnixMilli(), LoadW: 1000,
						PVKnown: pvKnown, LoadKnown: true, PVQuality: "test", LoadQuality: "test", PVBand: band, LoadBand: band, NetBand: band}
					f.errors = append(f.errors, forecasting.ErrorSample{Series: "champion", ConfigVersion: f.site().Revision, IssueID: "history",
						OriginMS: issued.UnixMilli(), IssuedAtMS: issued.UnixMilli(), StartMS: p.StartMS, EndMS: p.EndMS,
						AvailableAtMS: end.UnixMilli(), Lead: forecasting.LeadBucket(issued.UnixMilli(), p.StartMS),
						LoadErrorW: 200, PVKnown: pvKnown, LoadKnown: true, Prediction: p})
					if err := f.errors[len(f.errors)-1].Validate(); err != nil {
						t.Fatal(err)
					}
				}
			}
			in := f.Snapshot(origin, nil)
			base := trackerSlots(origin.Add(2*time.Hour), 1)
			base[0].PVW, base[0].LoadW = 0, 1000
			base = in.Resolve(context.Background(), base)
			planning := append([]mpc.Slot(nil), base...)
			in.Risk(base, planning, 2)
			if planning[0].LoadW != 1400 || planning[0].PVW != 0 {
				t.Fatalf("calibrated error was ignored or counted twice: %+v", planning[0])
			}
		})
	}
}

func TestForecastLoadRiskBeforeNetCalibration(t *testing.T) {
	at := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                    string
		upper                   *float64
		pv, k, wantLoad, wantPV float64
	}{
		{"night_model_upper", hostForecastPtr(4000.0), 0, 2, 7000, 0},
		{"night_without_model_bounds", nil, 0, 1, 1350, 0},
		{"narrow_model_keeps_cold_margin", hostForecastPtr(1100.0), 0, 1, 1350, 0},
		{"day_both_signals", hostForecastPtr(4000.0), 2000, 1, 4000, -1000},
		{"risk_disabled", hostForecastPtr(4000.0), 2000, 0, 1000, -2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := primaryFixture(at, func(_ context.Context, payload []byte) ([]byte, error) {
				_, reply := hostForecastReply(payload)
				for _, row := range reply["predictions"].([]any) {
					load := map[string]any{"known": true, "point_w": 1000, "quality": "learning", "uncertainty": "unavailable", "coverage": 0.5}
					if tc.upper != nil {
						load["lower_w"], load["upper_w"], load["uncertainty"] = 500, *tc.upper, "provisional"
					}
					row.(map[string]any)["load"] = load
					row.(map[string]any)["pv"] = map[string]any{"known": true, "point_w": tc.pv, "quality": "learning", "uncertainty": "unavailable", "coverage": 0.5}
				}
				return json.Marshal(reply)
			})
			in := f.Snapshot(at, trackerWeather(at, at))
			base := in.Resolve(context.Background(), trackerSlots(at, 1))
			unchanged := append([]mpc.Slot(nil), base...)
			planning := append([]mpc.Slot(nil), base...)
			in.Risk(base, planning, tc.k)
			if math.Abs(planning[0].LoadW-tc.wantLoad) > 1e-9 || planning[0].PVW != tc.wantPV {
				t.Fatalf("load/PV risk = %v/%v, want %v/%v", planning[0].LoadW, planning[0].PVW, tc.wantLoad, tc.wantPV)
			}
			if !reflect.DeepEqual(base, unchanged) {
				t.Fatal("risk changed the issued forecast")
			}
			in.Record(base, planning, "load-risk", at.UnixMilli())
			issue := (<-f.queue).issue
			if primarySeries(t, issue, "champion").Points[0].LoadW != 1000 || primarySeries(t, issue, "planning").Points[0].LoadW != tc.wantLoad {
				t.Fatal("archive lost the difference between forecast and planning margin")
			}
		})
	}
}
