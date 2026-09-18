package pvmodel

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestPVSamplingFreshZeroMissingStaleCurtailment(t *testing.T) {
	at := time.Now()
	for _, test := range []struct {
		name                                     string
		zero, offline, missing, stale, curtailed bool
		want                                     bool
	}{
		{name: "fresh", want: true}, {name: "healthy_zero", zero: true, want: true},
		{name: "offline", offline: true}, {name: "missing", missing: true}, {name: "stale", stale: true}, {name: "curtailed", curtailed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tel := telemetry.NewStore()
			w := -8000.0
			if test.zero {
				w = 0
			}
			tel.Update("pv", telemetry.DerPV, w, nil, nil)
			tel.RecordDriverSuccess("pv")
			if test.offline {
				tel.WatchdogScan(-time.Nanosecond)
			}
			s := NewService(nil, tel, func(time.Time) float64 { return 700 }, func(time.Time) (float64, bool) { return 0, true }, 1000)
			s.CurtailmentActive = func() bool { return test.curtailed }
			if test.missing {
				s.SetForecastOptions(telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "missing-pv", DerType: telemetry.DerPV}}})
			}
			now := time.Now()
			if test.stale {
				now = at.Add(5 * time.Minute)
			}
			s.sampleAt(now)
			if got := s.Model().Samples > 0; got != test.want {
				t.Fatalf("trained=%v want=%v", got, test.want)
			}
		})
	}
}

func TestLegacyPVLongStreamStaysFiniteAndLearning(t *testing.T) {
	m := NewModel(1000)
	start := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	accepted := 0
	for i := 0; i < 100000; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		if m.Update(800, 0, at, 8000) {
			accepted++
		}
		if i > 1000 && i%1000 == 0 {
			if p := m.Predict(800, 0, at); !finite(p) || math.Abs(p-8000) > 1600 {
				t.Fatalf("unstable stream at %d: %.0fW", i, p)
			}
		}
	}
	if accepted < 95000 {
		t.Fatalf("steady stream stopped learning: %d accepted", accepted)
	}
	for _, row := range m.P {
		for _, v := range row {
			if !finite(v) {
				t.Fatal("nonfinite covariance")
			}
		}
	}
}

func TestLegacyZeroScaleAndIndependentCoverageSurviveRestart(t *testing.T) {
	db := openTestDB(t)
	s := NewService(db, nil, nil, nil, 10000)
	s.SetACLimit(11000)
	at := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	for day := 0; day < 8; day++ {
		s.model.Update(800, 0, at.Add(time.Duration(day)*24*time.Hour), 0)
	}
	s.persist()
	restored := NewService(db, nil, nil, nil, 10000)
	if restored.Model() != s.Model() {
		t.Fatal("restart lost learned state")
	}
	if p := restored.Model().Predict(800, 0, at.Add(8*24*time.Hour)); p > 1 {
		t.Fatalf("known zero treated as missing scale: %.0fW", p)
	}
	if restored.Model().Trust(at.Add(8*24*time.Hour)) < 1 {
		t.Fatal("restart lost independent day coverage")
	}
}

func TestLegacyNumericalInputsDoNotPoisonState(t *testing.T) {
	m := NewModel(5000)
	at := time.Now()
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if m.Update(v, 0, at, 1000) || m.Update(800, v, at, 1000) || m.Update(800, 0, at, v) {
			t.Fatal("nonfinite input accepted")
		}
	}
	m.P[1][1] = math.Inf(1)
	if !m.Update(800, 0, at, 4000) {
		t.Fatal("damaged covariance prevented recovery")
	}
	if !finite(m.Predict(800, 0, at)) {
		t.Fatal("damaged covariance poisoned prediction")
	}
}

func TestLegacyUnknownScaleCoverageAndRegime(t *testing.T) {
	at := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	m := NewModel(1000)
	if !m.Update(800, 0, at, 8000) {
		t.Fatal("guessed nameplate rejected real scale")
	}
	for minute := 1; minute < 60; minute++ {
		m.Update(800, 0, at.Add(time.Duration(minute)*time.Minute), 8000)
	}
	if m.Trust(at) > 1.0/7 {
		t.Fatal("correlated minute samples granted full trust")
	}
	if m.Trust(at.Add(6*time.Hour)) != 0 {
		t.Fatal("morning granted afternoon trust")
	}
	for day := 1; day < 10; day++ {
		for minute := 0; minute < 60; minute++ {
			m.Update(800, 0, at.Add(time.Duration(day)*24*time.Hour+time.Duration(minute)*time.Minute), 8000)
		}
	}
	start := at.Add(10 * 24 * time.Hour)
	accepted := 0
	for minute := 0; minute < 90; minute++ {
		if m.Update(800, 0, start.Add(time.Duration(minute)*time.Minute), 1000) {
			accepted++
		}
	}
	if accepted < 20 {
		t.Fatalf("regime remains locked out: %d accepted", accepted)
	}
	// A new site-wide scale needs support on separate days; short-term bias
	// has its own residual correction and must not resize tomorrow's prior.
	for day := 1; day < 8; day++ {
		for minute := 0; minute < 60; minute++ {
			m.Update(800, 0, start.Add(time.Duration(day)*24*time.Hour+time.Duration(minute)*time.Minute), 1000)
		}
	}
	if got := m.Predict(800, 0, start.Add(8*24*time.Hour)); got > 4000 {
		t.Fatalf("regime failed to lower structural forecast: %.0f", got)
	}
}

func TestLegacySingleHealthyZeroDoesNotEraseGlobalScale(t *testing.T) {
	m := NewModel(8000)
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	before := m.Predict(800, 0, at.Add(24*time.Hour))
	if !m.Update(800, 0, at, 0) {
		t.Fatal("healthy zero should train")
	}
	after := m.Predict(800, 0, at.Add(24*time.Hour))
	if after < before*.8 || m.InferredScaleKnown {
		t.Fatalf("one zero erased the whole-site scale: %.0f -> %.0f", before, after)
	}
}

func TestLegacyCloudRegimeDoesNotResizePlant(t *testing.T) {
	m := NewModel(8000)
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		m.Update(800, 0, at.Add(time.Duration(i)*time.Minute), 6400)
	}
	for i := 60; i < 80; i++ {
		m.Update(800, 100, at.Add(time.Duration(i)*time.Minute), 1000)
	}
	if p := m.Predict(800, 0, at.Add(24*time.Hour)); p > 6400*1.1 {
		t.Fatalf("cloud error resized plant: %.0fW", p)
	}
	if m.InferredScaleW > 8000 {
		t.Fatalf("overcast inferred %.0fW of capacity", m.InferredScaleW)
	}
}

func TestLegacyCoverageAgesAcrossSeasons(t *testing.T) {
	m := NewModel(8000)
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		m.Update(800, 0, at.AddDate(0, 0, i), 6400)
	}
	if m.Trust(at.AddDate(0, 0, 8)) < 0.99 {
		t.Fatal("fresh independent coverage lost")
	}
	if m.Trust(at.AddDate(0, 0, 190)) > 0.01 {
		t.Fatal("previous season still grants full trust")
	}
}
