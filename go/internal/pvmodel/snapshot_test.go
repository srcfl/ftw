package pvmodel

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestForecastSnapshotFreezesWholeHorizon(t *testing.T) {
	s := NewService(nil, nil, nil, nil, 5000)
	origin := time.Date(2026, 6, 15, 11, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		s.Residuals.Add(origin.Add(-time.Duration(i)*time.Minute), 1000, 1500)
	}
	s.model.LastMs = origin.UnixMilli()
	snapshot := s.ForecastSnapshot()
	for i := 0; i < 30; i++ {
		s.Residuals.Add(origin.Add(time.Duration(i)*time.Minute), 1000, 2000)
	}
	s.SetRated(12000)
	want := snapshot.Structural(origin, 700, 20)
	if snapshot.ResidualCorrect(origin, origin.Add(15*time.Minute), want) != 500 {
		t.Fatal("later residuals changed issued snapshot")
	}
	if snapshot.Revision == s.ForecastSnapshot().Revision {
		t.Fatal("state changed without a new revision")
	}
	if snapshot.LatestInput != origin {
		t.Fatalf("latest input %v", snapshot.LatestInput)
	}
	var restored ForecastSnapshot
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Structural(origin, 700, 20) != want || restored.ResidualCorrect(origin, origin.Add(time.Hour), want) != snapshot.ResidualCorrect(origin, origin.Add(time.Hour), want) || restored.Revision != snapshot.Revision {
		t.Fatal("serialized inference cannot replay")
	}
	// The capture time is not a learned model revision.
	if s.ForecastSnapshot().Revision != s.ForecastSnapshot().Revision {
		t.Fatal("unchanged inference state got a new revision")
	}
}

func TestForecastSnapshotResidualsExcludeFutureAndExpire(t *testing.T) {
	s := NewService(nil, nil, nil, nil, 5000)
	origin := time.Date(2026, 6, 15, 11, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		s.Residuals.Add(origin.Add(time.Duration(i+1)*time.Second), 1000, 1500)
	}
	p := s.ForecastSnapshot()
	if p.ResidualCorrect(origin, origin.Add(time.Minute), 1000) != 0 {
		t.Fatal("future labels leaked into correction")
	}
	later := origin.Add(time.Minute)
	if p.ResidualCorrect(later, later.Add(15*time.Minute), 1000) != 500 {
		t.Fatal("available observations not used")
	}
	if p.ResidualCorrect(origin.Add(3*time.Hour), origin.Add(4*time.Hour), 1000) != 0 {
		t.Fatal("stale residuals did not expire")
	}
}

func TestForecastSnapshotMatchesLegacyCorrection(t *testing.T) {
	s := NewService(nil, nil, nil, nil, 5000)
	origin := time.Now()
	for i := 0; i < 30; i++ {
		s.Residuals.Add(origin.Add(-time.Duration(i)*time.Minute), 1000, 1500+float64(i*10))
	}
	p := s.ForecastSnapshot()
	for _, minutes := range []int{0, 15, 30, 60, 120, 180} {
		target := origin.Add(time.Duration(minutes) * time.Minute)
		if math.Abs(p.ResidualCorrect(origin, target, 1000)-s.ResidualCorrect(origin, target, 1000)) > 1e-9 {
			t.Fatalf("legacy behavior changed at horizon %dm", minutes)
		}
	}
}

func TestReconfigureRejectsOldSiteSample(t *testing.T) {
	db := openTestDB(t)
	entered, release := make(chan struct{}), make(chan struct{})
	tel := telemetry.NewStore()
	tel.Update("pv", telemetry.DerPV, -4000, nil, nil)
	tel.RecordDriverSuccess("pv")
	s := NewService(db, tel, func(time.Time) float64 { close(entered); <-release; return 800 }, func(time.Time) (float64, bool) { return 0, true }, 5000)
	s.Residuals.Add(time.Now(), 1000, 2000)
	done := make(chan struct{})
	go func() { s.sampleAt(time.Now()); close(done) }()
	<-entered
	s.Reconfigure(func(time.Time) float64 { return 400 })
	close(release)
	<-done
	if m := s.Model(); m.Samples != 0 || s.Residuals.Len() != 0 {
		t.Fatal("old-site sample survived reset")
	}
	if got := s.PredictStructural(time.Now(), 0); got != 2000 {
		t.Fatalf("new location callback not applied: %.0f", got)
	}
	restored := NewService(db, nil, nil, nil, 5000)
	if restored.Model().Samples != 0 {
		t.Fatal("site reset not persisted")
	}
}

func TestReconfigurePersistsRevisionWithModel(t *testing.T) {
	db := openTestDB(t)
	cs := func(time.Time) float64 { return 800 }
	s := NewService(db, nil, cs, nil, 5000)
	s.Reconfigure(cs, "site-v1")
	s.model.Update(800, 0, time.Now(), 4000)
	s.persist()
	restored := NewService(db, nil, cs, nil, 5000)
	restored.Reconfigure(cs, "site-v1")
	if m := restored.Model(); m.Samples != 1 || m.ConfigRevision != "site-v1" {
		t.Fatal("same config discarded bound state")
	}
	restored.Reconfigure(cs, "site-v2")
	if m := restored.Model(); m.Samples != 0 || m.ConfigRevision != "site-v2" {
		t.Fatal("new config retained old training")
	}
	last := NewService(db, nil, cs, nil, 5000).Model()
	if last.ConfigRevision != "site-v2" || last.Samples != 0 {
		t.Fatal("revision and clean model were not persisted together")
	}
}

func BenchmarkLegacyPVUpdate(b *testing.B) {
	m := NewModel(5000)
	at := time.Date(2026, 6, 15, 11, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Update(700, 20, at.Add(time.Duration(i)*time.Minute), 3000)
	}
	b.ReportMetric(float64(m.Samples)/float64(b.N), "accepted/op")
}

func BenchmarkLegacyPVSnapshotHorizon(b *testing.B) {
	s := NewService(nil, nil, nil, nil, 5000)
	origin := time.Now()
	for i := 0; i < 240; i++ {
		s.Residuals.Add(origin.Add(-time.Duration(240-i)*30*time.Second), 1000, 1500)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := s.ForecastSnapshot()
		for q := 0; q < 192; q++ {
			at := origin.Add(time.Duration(q) * 15 * time.Minute)
			base := p.Structural(at, 700, 20)
			p.ResidualCorrect(origin, at, base)
		}
	}
}
