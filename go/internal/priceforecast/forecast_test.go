package priceforecast

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestFreshModelHasSensibleCurve(t *testing.T) {
	// Untrained model returns baked-in typical Nordic pattern:
	// midday trough, morning + evening peaks. Tests shape, not exact values.
	m := NewZoneModel("SE3")
	midday := time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 6, 15, 19, 0, 0, 0, time.UTC)
	overnight := time.Date(2026, 6, 15, 3, 0, 0, 0, time.UTC)
	pm := m.Predict(midday)
	pe := m.Predict(evening)
	po := m.Predict(overnight)
	if !(pe > pm) {
		t.Errorf("evening (%.1f) should exceed midday (%.1f)", pe, pm)
	}
	if !(pm < po) {
		t.Errorf("midday (%.1f) should be below overnight (%.1f) due to solar flood", pm, po)
	}
	// Winter vs summer seasonality
	wintr := time.Date(2026, 1, 15, 19, 0, 0, 0, time.UTC)
	smrEv := time.Date(2026, 7, 15, 19, 0, 0, 0, time.UTC)
	if !(m.Predict(wintr) > m.Predict(smrEv)) {
		t.Errorf("winter (%.1f) should exceed summer (%.1f)", m.Predict(wintr), m.Predict(smrEv))
	}
}

func TestFitsHourOfWeekPattern(t *testing.T) {
	// Synthetic: SE3 prices with morning peak 150, midday trough 30,
	// evening peak 200. Two years of data so the Bayesian prior (weight
	// ≈ 8) is swamped by ~100 samples per hour-of-week bucket.
	var pts []state.PricePoint
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for d := 0; d < 730; d++ { // 2 years
		for h := 0; h < 24; h++ {
			ts := start.Add(time.Duration(d*24+h) * time.Hour)
			var price float64
			switch {
			case h >= 7 && h <= 9:
				price = 150
			case h >= 11 && h <= 14:
				price = 30
			case h >= 17 && h <= 20:
				price = 200
			default:
				price = 80
			}
			pts = append(pts, state.PricePoint{
				Zone:       "SE3",
				SlotTsMs:   ts.UnixMilli(),
				SlotLenMin: 60,
				SpotOreKwh: price,
			})
		}
	}
	m := NewZoneModel("SE3")
	m.FitFromHistory(pts)

	// With ~100+ samples per bucket, fit should be very close to data.
	// Tolerance generous because month multipliers still apply some
	// seasonal scaling.
	mornMon := time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)
	if got := m.Predict(mornMon); math.Abs(got-150) > 20 {
		t.Errorf("Mon 08:00 peak: got %f, want ~150 (±20)", got)
	}
	trough := time.Date(2026, 3, 4, 13, 0, 0, 0, time.UTC)
	if got := m.Predict(trough); math.Abs(got-30) > 20 {
		t.Errorf("Wed 13:00 trough: got %f, want ~30 (±20)", got)
	}
	eve := time.Date(2026, 3, 6, 19, 0, 0, 0, time.UTC)
	if got := m.Predict(eve); math.Abs(got-200) > 20 {
		t.Errorf("Fri 19:00 peak: got %f, want ~200 (±20)", got)
	}
}

func TestSparseHistoryFallsBackToPriorShape(t *testing.T) {
	// Only 3 days of data. The Bayesian prior (weight 8) dominates,
	// so the predictions should still show the baked hour-of-week
	// shape — morning + evening peaks, midday trough — even if the
	// short training sample happened to be uniform.
	var pts []state.PricePoint
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	for d := 0; d < 3; d++ {
		for h := 0; h < 24; h++ {
			ts := start.Add(time.Duration(d*24+h) * time.Hour)
			pts = append(pts, state.PricePoint{
				Zone: "SE3", SlotTsMs: ts.UnixMilli(),
				SlotLenMin: 60, SpotOreKwh: 100, // totally flat — unusual
			})
		}
	}
	m := NewZoneModel("SE3")
	m.FitFromHistory(pts)

	// Even though training data was flat, shape persists from prior.
	morn := time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)
	midday := time.Date(2026, 3, 2, 13, 0, 0, 0, time.UTC)
	eve := time.Date(2026, 3, 2, 19, 0, 0, 0, time.UTC)
	if !(m.Predict(morn) > m.Predict(midday)) {
		t.Errorf("morning (%f) should beat midday (%f) — prior shape lost",
			m.Predict(morn), m.Predict(midday))
	}
	if !(m.Predict(eve) > m.Predict(midday)) {
		t.Errorf("evening (%f) should beat midday (%f) — prior shape lost",
			m.Predict(eve), m.Predict(midday))
	}
}

func TestSeedFromCSVIngestsAndFits(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	csv := `zone,slot_ts_ms,slot_len_min,spot_ore_kwh
SE3,1735689600000,60,50.0
SE3,1735693200000,60,60.0
SE3,1735696800000,60,70.0
SE3,1735700400000,60,80.0
SE4,1735689600000,60,90.0
`
	// Write to a tempfile so SeedFromCSV sees a real path.
	s := NewService(st, []string{"SE3", "SE4"})
	n, err := s.ingestCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if n != 5 {
		t.Errorf("want 5 rows imported, got %d", n)
	}
	// Verify SE3 data landed in the store.
	rows, err := st.LoadPrices("SE3", 0, 3000000000000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Errorf("SE3 rows: got %d want 4", len(rows))
	}
}

// TestPredictStableAcrossDST ensures Predict returns the same value for
// the same absolute instant regardless of the timezone the caller has
// attached to the time.Time struct. Before the UTC coercion in
// hourOfWeek + Predict, passing a local-zone time around DST boundaries
// produced a different bucket (and thus price) than passing the UTC
// equivalent — Erik's 21:00 bug was on this exact code path.
func TestPredictStableAcrossDST(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skipf("Europe/Stockholm tzdata unavailable: %v", err)
	}
	m := NewZoneModel("SE3")
	// Several points over the year — including both DST transitions.
	cases := []struct {
		name string
		inst time.Time
	}{
		// Winter (CET = UTC+1): 19:00 local = 18:00 UTC
		{"winter evening", time.Date(2026, 1, 15, 18, 0, 0, 0, time.UTC)},
		// Spring-forward day: 2026-03-29 01:00 UTC = 03:00 CEST (02:00 local skipped)
		{"spring forward 01UTC", time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC)},
		{"spring forward 10UTC", time.Date(2026, 3, 29, 10, 0, 0, 0, time.UTC)},
		// Summer (CEST = UTC+2): 19:00 local = 17:00 UTC
		{"summer evening", time.Date(2026, 7, 15, 17, 0, 0, 0, time.UTC)},
		// Fall-back day: 2026-10-25 00:00 UTC = 02:00 CEST; 01:00 UTC = 02:00 CET (second time)
		{"fall back 00UTC", time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)},
		{"fall back 01UTC", time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)},
		// Erik's scenario: ~21:00 local (19:00-20:00 UTC depending on season)
		{"winter 21 local", time.Date(2026, 12, 10, 20, 0, 0, 0, time.UTC)},
		{"summer 21 local", time.Date(2026, 7, 10, 19, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			utc := tc.inst
			local := utc.In(stockholm)
			if !utc.Equal(local) {
				t.Fatalf("instants must be equal — test bug")
			}
			pUTC := m.Predict(utc)
			pLocal := m.Predict(local)
			if pUTC != pLocal {
				t.Errorf("Predict diverged across timezones for same instant: "+
					"utc=%v -> %.4f, local=%v -> %.4f",
					utc, pUTC, local, pLocal)
			}
		})
	}
}

// TestHourOfWeekStableAcrossDST is the lower-level regression: the
// bucket index itself must not change when the same instant is
// represented in a different timezone.
func TestHourOfWeekStableAcrossDST(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skipf("Europe/Stockholm tzdata unavailable: %v", err)
	}
	// Pick a few instants across DST boundaries.
	instants := []time.Time{
		time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC), // spring forward
		time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC), // fall back
		time.Date(2026, 7, 15, 17, 0, 0, 0, time.UTC), // summer
		time.Date(2026, 12, 15, 20, 0, 0, 0, time.UTC), // winter
	}
	for _, inst := range instants {
		utc := inst
		local := inst.In(stockholm)
		if hourOfWeek(utc) != hourOfWeek(local) {
			t.Errorf("hourOfWeek differs: utc=%d local=%d (inst=%v)",
				hourOfWeek(utc), hourOfWeek(local), inst)
		}
	}
}

func TestSeedFromCSVRejectsMissingColumns(t *testing.T) {
	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	s := NewService(st, []string{"SE3"})
	_, err := s.ingestCSV(strings.NewReader("zone,timestamp\nSE3,1000\n"))
	if err == nil {
		t.Error("expected error for missing spot_ore_kwh column")
	}
}
