package loadmodel

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/modelstate"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// feed trains m once per service interval in [from, to).
func feed(m *Model, from, to time.Time, sample func(time.Time) (loadW, tempC float64)) {
	for at := from; at.Before(to); at = at.Add(defaultSampleInterval) {
		loadW, tempC := sample(at)
		m.Update(at, loadW, tempC)
	}
}

// #1491: the home box's prior said 300 W/°C while the house drew 400 W at
// 10 °C. The prior's 2,400 W of heat sat above every reading, so no hour
// trained and the coefficient never moved.
func TestHeatingBoundLowersAnOverstatedPrior(t *testing.T) {
	const houseW, outdoorC = 400.0, 10.0
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	m := NewModel(5520)
	m.Timezone = stockholm.String()
	m.HeatingW_per_degC = 300
	monday := time.Date(2026, 1, 5, 0, 0, 0, 0, stockholm)
	saturday := monday.AddDate(0, 0, 5)
	night := saturday.Add(2 * time.Hour)

	// The untrained 02:00 hour predicts its 650 W prior plus 300 × 8 W of heat.
	if got := m.Predict(night, outdoorC) - houseW; math.Abs(got-2650.4) > 0.5 {
		t.Fatalf("error at 02:00 before training = %.1f W, want 2650.4", got)
	}

	var steps []string
	for at := monday; !at.After(saturday); at = at.Add(defaultSampleInterval) {
		before := m.HeatingW_per_degC
		m.Update(at, houseW, outdoorC)
		if m.HeatingW_per_degC != before {
			steps = append(steps, fmt.Sprintf("%s %g", at.Format("Mon 15:04"), m.HeatingW_per_degC))
		}
	}
	// Each day's bound is 400 W / 8 °C = 50 W/°C. Once three days have
	// closed, the coefficient moves halfway to it at each local midnight.
	if want := []string{"Thu 00:00 175", "Fri 00:00 112.5", "Sat 00:00 81.25"}; !slices.Equal(steps, want) {
		t.Fatalf("coefficient steps = %q, want %q", steps, want)
	}
	if got := m.Predict(night, outdoorC) - houseW; math.Abs(got-900.4) > 0.5 {
		t.Fatalf("error at 02:00 after five days = %.1f W, want 900.4", got)
	}
}

// A house that really heats draws 300 W plus 200 W/°C below 18 °C, with the
// outdoor temperature between 0 and 10 °C. The 300 W base keeps each day's
// bound above 200 W/°C, so the bound never takes the coefficient below the
// truth. In two weeks no bucket reaches the eight days the fit needs, so only
// the bound can move the coefficient here.
func TestHeatingBoundKeepsRealHeating(t *testing.T) {
	house := func(at time.Time) (float64, float64) {
		hour := float64(at.Hour()) + float64(at.Minute())/60
		tempC := 5 - 5*math.Cos(2*math.Pi*(hour-3)/24) // 0 °C at 03:00, 10 °C at 15:00
		return 300 + 200*(HeatingReferenceC-tempC), tempC
	}
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 14)
	for _, coef := range []float64{150, 200, 400} {
		m := NewModel(8000)
		m.HeatingW_per_degC = coef
		lowest := coef
		for at := start; !at.After(end); at = at.Add(defaultSampleInterval) {
			loadW, tempC := house(at)
			m.Update(at, loadW, tempC)
			lowest = math.Min(lowest, m.HeatingW_per_degC)
		}
		if coef <= 200 && m.HeatingW_per_degC != coef {
			t.Errorf("coefficient %.0f at or below the true 200 W/°C moved to %.2f", coef, m.HeatingW_per_degC)
		}
		if coef > 200 && (m.HeatingW_per_degC >= coef || lowest < 200) {
			t.Errorf("coefficient %.0f ended at %.2f, lowest %.2f: want lower but never below 200",
				coef, m.HeatingW_per_degC, lowest)
		}
	}
}

// The same house has its heating off for one day, and draws only its 300 W
// base. That day's bound is about 23 W/°C, but the days around it keep the
// highest of the last three above the true coefficient.
func TestHeatingBoundIgnoresOneUnusualDay(t *testing.T) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	off := start.AddDate(0, 0, 4)
	m := NewModel(8000)
	m.HeatingW_per_degC = 200
	feed(m, start, start.AddDate(0, 0, 14), func(at time.Time) (float64, float64) {
		hour := float64(at.Hour()) + float64(at.Minute())/60
		tempC := 5 - 5*math.Cos(2*math.Pi*(hour-3)/24)
		if at.Truncate(24 * time.Hour).Equal(off) {
			return 300, tempC
		}
		return 300 + 200*(HeatingReferenceC-tempC), tempC
	})
	if m.HeatingW_per_degC != 200 {
		t.Fatalf("one day without heating moved the coefficient to %.2f", m.HeatingW_per_degC)
	}
}

func TestHeatingBoundIgnoresMildDays(t *testing.T) {
	m := NewModel(5520)
	m.HeatingW_per_degC = 300
	start := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	feed(m, start, start.AddDate(0, 0, 3).Add(defaultSampleInterval), func(at time.Time) (float64, float64) {
		return 400, 15 + float64(at.Hour())/2 // 15 °C to 26.5 °C
	})
	if m.HeatingW_per_degC != 300 {
		t.Errorf("mild days moved the coefficient to %.2f", m.HeatingW_per_degC)
	}
	if m.HeatingBoundSamples != 0 || m.HeatingBoundLoadSumW != 0 || m.HeatingBoundDeltaSumC != 0 {
		t.Errorf("mild samples recorded: %d samples, %.0f W, %.0f °C",
			m.HeatingBoundSamples, m.HeatingBoundLoadSumW, m.HeatingBoundDeltaSumC)
	}
}

func TestHeatingBoundNeedsSixColdHours(t *testing.T) {
	m := NewModel(5520)
	m.HeatingW_per_degC = 300
	monday := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	thursday := monday.AddDate(0, 0, 3)
	friday := monday.AddDate(0, 0, 4)
	// Monday is one sample short of six cold hours at 400 W. Tuesday to
	// Thursday each have six at 800 W. The rest of each day is mild.
	sample := func(at time.Time) (float64, float64) {
		sinceMidnight := at.Sub(time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC))
		switch {
		case at.Weekday() == time.Monday && sinceMidnight < 6*time.Hour-defaultSampleInterval:
			return 400, 10
		case at.Weekday() != time.Monday && sinceMidnight < 6*time.Hour:
			return 800, 10
		}
		return 400, 16
	}

	// The first sample of a day closes the day before. Had Monday counted,
	// Thursday's first sample would close a third cold day.
	feed(m, monday, thursday.Add(defaultSampleInterval), sample)
	if m.HeatingW_per_degC != 300 || m.HeatingBoundRecentN != 2 {
		t.Fatalf("%d cold samples counted as a day: coefficient %.2f, %d days",
			heatingBoundMinSamples-1, m.HeatingW_per_degC, m.HeatingBoundRecentN)
	}
	feed(m, thursday.Add(defaultSampleInterval), friday.Add(defaultSampleInterval), sample)
	// Each bound is 800 W / 8 °C = 100 W/°C.
	if m.HeatingW_per_degC != 200 {
		t.Fatalf("three days of %d cold samples left the coefficient at %.2f, want 200", heatingBoundMinSamples, m.HeatingW_per_degC)
	}
}

func TestHeatingBoundSumsSurviveRestart(t *testing.T) {
	st := openTestDB(t)
	s := NewService(st, telemetry.NewStore(), "site", 4000, 0)
	monday := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	s.mu.Lock()
	m := s.activeModelLocked()
	m.HeatingW_per_degC = 300
	feed(m, monday, monday.AddDate(0, 0, 2).Add(7*time.Hour), func(time.Time) (float64, float64) { return 400, 10 })
	s.mu.Unlock()
	if err := s.persist(); err != nil {
		t.Fatal(err)
	}

	got := NewService(st, telemetry.NewStore(), "site", 4000, 0).Model()

	if got.HeatingBoundDay != "2026-01-07" || got.HeatingBoundSamples != 420 ||
		got.HeatingBoundLoadSumW != 420*400 || got.HeatingBoundDeltaSumC != 420*8 ||
		got.HeatingBoundRecentN != 2 || got.HeatingBoundRecent != [heatingBoundDays]float64{0, 50, 50} {
		t.Fatalf("day sums after restart: day %q, %d samples, %.0f W, %.0f °C, bounds %v (%d)",
			got.HeatingBoundDay, got.HeatingBoundSamples, got.HeatingBoundLoadSumW, got.HeatingBoundDeltaSumC,
			got.HeatingBoundRecent, got.HeatingBoundRecentN)
	}
	if got != s.Model() {
		t.Fatal("restart changed the model")
	}
}

// Boxes hold state written before the bound existed, under the same feature
// hash. It must load as it is, with empty day sums.
func TestStateWithoutHeatingBoundStillLoads(t *testing.T) {
	st := openTestDB(t)
	raw, err := json.Marshal(trainedModel(t))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		if strings.HasPrefix(key, "heating_bound_") {
			delete(fields, key)
		}
	}
	js, err := modelstate.Wrap(FeatureHash(), fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(stateKey(ProfileHome), js); err != nil {
		t.Fatal(err)
	}

	got := NewService(st, telemetry.NewStore(), "site", 4000, 0).Model()

	if got.Samples != 12 || got.HeatingW_per_degC != 275 {
		t.Fatalf("old state not restored: samples %d, heating %.0f", got.Samples, got.HeatingW_per_degC)
	}
	if got.HeatingBoundDay != "" || got.HeatingBoundSamples != 0 || got.HeatingBoundRecentN != 0 {
		t.Fatalf("old state gained day sums: day %q, %d samples, %d days",
			got.HeatingBoundDay, got.HeatingBoundSamples, got.HeatingBoundRecentN)
	}
}
