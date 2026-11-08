package mpc

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestAnchorPVToMeasured(t *testing.T) {
	start := time.Date(2026, 11, 8, 10, 30, 0, 0, time.UTC).UnixMilli()
	const quarter = int64(15 * time.Minute / time.Millisecond)
	slots := func(pv ...float64) []Slot {
		out := make([]Slot, len(pv))
		for i, w := range pv {
			out[i] = Slot{StartMs: start + int64(i)*quarter, LenMin: 15, PVW: -w, LoadW: 1000}
		}
		return out
	}
	// The stub records the in-flight forecast and moves every forecast to a
	// value that identifies which inputs it received.
	move := func(inflightW, forecastW float64, dt time.Duration) float64 {
		return forecastW - inflightW/2
	}
	check := func(t *testing.T, got []Slot, want ...float64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("slots %d, want %d", len(got), len(want))
		}
		for i, w := range want {
			if math.Abs(-got[i].PVW-w) > 1e-9 {
				t.Fatalf("slot %d: got %v, want %v", i, -got[i].PVW, w)
			}
		}
	}

	t.Run("moves every slot holding forecast PV", func(t *testing.T) {
		got := slots(4000, 3000, 2000)
		anchorPVToMeasured(got, start+quarter/2, move)
		check(t, got, 2000, 1000, 0)
	})
	t.Run("keeps a slot with no forecast PV at zero", func(t *testing.T) {
		got := slots(4000, 0, 2000)
		anchorPVToMeasured(got, start+quarter/2, move)
		check(t, got, 2000, 0, 0)
	})
	t.Run("uses the slot midpoint for the fade", func(t *testing.T) {
		var dts []time.Duration
		got := slots(4000, 3000)
		anchorPVToMeasured(got, start+7*int64(time.Minute/time.Millisecond), func(_, f float64, dt time.Duration) float64 {
			dts = append(dts, dt)
			return f
		})
		if dts[0] != 30*time.Second || dts[1] != 15*time.Minute+30*time.Second {
			t.Fatalf("slot midpoints: %v", dts)
		}
	})
	t.Run("an hourly in-flight slot keeps the forecast", func(t *testing.T) {
		got := slots(4000, 3000)
		got[0].LenMin = 60
		anchorPVToMeasured(got, start+quarter/2, move)
		check(t, got, 4000, 3000)
	})
	t.Run("nil keeps the forecast", func(t *testing.T) {
		got := slots(4000, 3000)
		anchorPVToMeasured(got, start+quarter/2, nil)
		check(t, got, 4000, 3000)
	})
	t.Run("no in-flight slot keeps the forecast", func(t *testing.T) {
		got := slots(4000, 3000)
		anchorPVToMeasured(got, start-time.Minute.Milliseconds(), move)
		check(t, got, 4000, 3000)
		got = slots(4000, 3000)
		anchorPVToMeasured(got, start+quarter, move)
		check(t, got, 4000, 3000)
	})
	t.Run("non-finite in-flight forecast keeps the forecast", func(t *testing.T) {
		got := slots(4000, 3000)
		got[0].PVW = math.NaN()
		anchorPVToMeasured(got, start+quarter/2, move)
		if !math.IsNaN(got[0].PVW) || got[1].PVW != -3000 {
			t.Fatalf("got %+v", got[:2])
		}
	})
}

// Energyplan's worker sees no measured PV, so the nowcast has to run on the
// forecast it selected. The stub halves whatever PV it is given.
func TestReplanAnchorsThePlanAfterTheForecastIsSelected(t *testing.T) {
	svc := shadowTestService(t)
	start := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 4; i++ {
		if err := svc.Store.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "test", FetchedAtMs: start.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	svc.now = func() time.Time { return start.Add(5 * time.Minute) }
	var archived []Slot
	svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
		return ForecastInputs{Load: func(time.Time) float64 { return 1000 },
			Record: func(base, _ []Slot, _ []bool, _ string, _ int64) { archived = base },
			Resolve: func(_ context.Context, base []Slot) []Slot {
				out := append([]Slot(nil), base...)
				for i := range out {
					out[i].PVW = -4000
				}
				return out
			},
			PVNowcast: func(_, forecastW float64, _ time.Duration) float64 { return forecastW / 2 },
		}
	}
	plan := svc.Replan(context.Background())
	svc.shadowWG.Wait()
	if plan == nil || len(plan.Actions) == 0 {
		t.Fatalf("no plan: %+v", svc.PlanSnapshot())
	}
	if got := -plan.Actions[0].PVW; math.Abs(got-2000) > 1e-6 {
		t.Fatalf("first action PV %v, want the selected forecast halved", got)
	}
	if got := -*plan.Actions[0].ForecastPVW; math.Abs(got-2000) > 1e-6 {
		t.Fatalf("published point forecast %v, want the anchored value", got)
	}
	if len(archived) == 0 || archived[0].PVW != -4000 {
		t.Fatalf("archive scores the nowcast instead of the selected forecast: %+v", archived)
	}
}
