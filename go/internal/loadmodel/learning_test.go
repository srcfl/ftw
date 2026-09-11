package loadmodel

import (
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"testing"
	"time"
)

func TestIndependentDaysAreCadenceInvariant(t *testing.T) {
	start := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	var reference float64
	for _, cadence := range []time.Duration{time.Second, 10 * time.Second, time.Minute} {
		m := NewModel(4000)
		for day := 0; day < 10; day++ {
			for elapsed := time.Duration(0); elapsed < time.Hour; elapsed += cadence {
				m.Update(start.AddDate(0, 0, 7*day).Add(elapsed), 200+float64(day)*100, 20)
			}
		}
		if got := m.Bucket[3].Days; got != 10 {
			t.Fatalf("%s grants %d days", cadence, got)
		}
		predicted := m.Predict(start.AddDate(0, 0, 70), 20)
		if reference == 0 {
			reference = predicted
		} else if math.Abs(reference-predicted) > 0.001 {
			t.Fatalf("cadence changes structure: reference=%f got=%f", reference, predicted)
		}
	}
}
func TestSiteClockDSTRoutines(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	m := NewModel(4000)
	m.Timezone = loc.String()
	winter := time.Date(2026, 3, 23, 19, 0, 0, 0, loc)
	summer := time.Date(2026, 3, 30, 19, 0, 0, 0, loc)
	if m.hourOfWeek(winter.UTC()) != 19 || m.hourOfWeek(summer.UTC()) != 19 {
		t.Fatal("Monday evening moved with DST")
	}
	a := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)
	if m.hourOfWeek(a) != m.hourOfWeek(a.Add(time.Hour)) {
		t.Fatal("repeated local hour has two routine buckets")
	}
}
func TestUnknownTemperaturePreservesHeatAndSkipsFit(t *testing.T) {
	m := NewModel(4000)
	m.HeatingW_per_degC = 100
	at := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	m.Update(at, 2000, 0)
	known := m.Predict(at, 0)
	unknown := m.PredictNoTemp(at)
	if unknown != known {
		t.Fatalf("unknown removed heat: known=%f unknown=%f", known, unknown)
	}
	m.Update(at.Add(time.Minute), 2000, math.NaN())
	if m.HeatingW_per_degC != 100 || m.LastTemperatureC != 0 || !m.HasTemperature {
		t.Fatal("unknown trained temperature")
	}
}
func TestTimezoneCoverageTemperaturePersist(t *testing.T) {
	st := openTestDB(t)
	s := NewService(st, telemetry.NewStore(), "site", 4000, 11000)
	if err := s.SetTimezone("Europe/Stockholm"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	m := s.activeModelLocked()
	m.HeatingW_per_degC = 100
	at := time.Date(2026, 1, 5, 18, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		m.Update(at.AddDate(0, 0, 7*i), 2000, 0)
	}
	s.mu.Unlock()
	if err := s.persist(); err != nil {
		t.Fatal(err)
	}
	restored := NewService(st, telemetry.NewStore(), "site", 4000, 11000)
	if got := restored.Model(); got != s.Model() {
		t.Fatalf("restart changed state: zone=%s days=%d temp=%f", got.Timezone, got.Bucket[19].Days, got.LastTemperatureC)
	}
	if err := restored.SetTimezone("UTC"); err != nil {
		t.Fatal(err)
	}
	// A crash after writing the new timezone but before model persistence must
	// discard the old clock model, never reinterpret its existing buckets.
	again := NewService(st, telemetry.NewStore(), "site", 4000, 11000)
	if again.Model().Samples != 0 {
		t.Fatal("old zone data relabelled as UTC")
	}
}
func TestCoverageDoesNotClaimUnseenHoursOrSeasons(t *testing.T) {
	m := NewModel(4000)
	at := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		m.Update(at.AddDate(0, 0, 7*i), 100, 20)
	}
	last := at.AddDate(0, 0, 49)
	if m.Coverage(last) != 1 || m.Coverage(last.Add(time.Hour)) != 0 {
		t.Fatal("coverage ignored independent hour evidence")
	}
	if m.Coverage(last.AddDate(0, 0, 182)) >= 0.2 {
		t.Fatal("stale season still trusted")
	}
}
func BenchmarkLoadPredictSnapshot(b *testing.B) {
	m := NewModel(4000)
	m.Timezone = "Europe/Stockholm"
	at := time.Now()
	m.Predict(at, 5)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Predict(at, 5)
	}
}
