package pvmodel

import (
	"math"
	"testing"
	"time"
)

// On 8 Nov 2026 the plan at 10:41:55 still said 3,814 W for the 10:30 quarter
// while the panels made about 1,390 W. The residual window held mean 83 W and
// std 1,271 W over 84 samples: swings in both directions cancel to a small mean
// that the noise gate rejects. And once a slot's midpoint has passed, the
// correction is out of its horizon anyway.
func TestResidualCorrectionZeroOnSwingingMorning(t *testing.T) {
	now := time.Date(2026, 11, 8, 9, 41, 55, 0, time.UTC)
	b := NewResidualBuffer()
	snap := ForecastSnapshot{}
	for i := 0; i < 84; i++ {
		at := now.Add(-time.Duration(83-i) * time.Minute)
		residual := 1354.0
		if i%2 == 1 {
			residual = -1188
		}
		b.Add(at, 3000, 3000+residual)
		snap.Residuals = append(snap.Residuals, ResidualObservation{At: at, PredictedW: 3000, ActualW: 3000 + residual})
	}
	diag := b.Diag(now)
	if math.Abs(diag.MeanW-83) > 1e-9 || math.Abs(diag.StdW-1271) > 1e-9 {
		t.Fatalf("fixture drifted from the observed residuals: %+v", diag)
	}
	target := now.Add(10 * time.Minute)
	if got := b.Correct(now, target, 0); got != 0 {
		t.Fatalf("swinging residuals gave correction %v", got)
	}
	if got := snap.ResidualCorrect(now, target, 0); got != 0 {
		t.Fatalf("snapshot path gave correction %v", got)
	}

	// A steady bias would pass the gate, but not for a midpoint already passed.
	steady := NewResidualBuffer()
	for i := 0; i < 84; i++ {
		steady.Add(now.Add(-time.Duration(83-i)*time.Minute), 3000, 2000)
	}
	if got := steady.Correct(now, now.Add(time.Minute), 0); got != -1000 {
		t.Fatalf("steady bias want -1000, got %v", got)
	}
	if got := steady.Correct(now, now.Add(-4*time.Minute), 0); got != 0 {
		t.Fatalf("elapsed midpoint gave correction %v", got)
	}
}

func TestMeasuredNowW(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	at := func(minBefore float64, actual float64) ResidualObservation {
		return ResidualObservation{At: now.Add(-time.Duration(minBefore * float64(time.Minute))), PredictedW: 3000, ActualW: actual}
	}
	tests := []struct {
		name    string
		samples []ResidualObservation
		wantW   float64
		wantOK  bool
	}{
		{"mean of window", []ResidualObservation{at(4, 1000), at(2, 2000), at(0, 3000)}, 2000, true},
		{"too few samples", []ResidualObservation{at(4, 1000), at(1, 2000)}, 0, false},
		{"ignores older samples", []ResidualObservation{at(30, 9000), at(5.5, 9000), at(4, 1000), at(3, 1000), at(0, 1000)}, 1000, true},
		{"ignores future samples", []ResidualObservation{at(4, 1000), at(3, 1000), at(1, 1000), at(-1, 9000)}, 1000, true},
		{"skips non-finite", []ResidualObservation{at(4, math.NaN()), at(3, 1000), at(1, 1000)}, 0, false},
		{"empty", nil, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := ForecastSnapshot{Residuals: tc.samples}
			got, ok := s.MeasuredNowW(now)
			if ok != tc.wantOK || got != tc.wantW {
				t.Fatalf("got (%v,%v), want (%v,%v)", got, ok, tc.wantW, tc.wantOK)
			}
		})
	}
}

func TestNowcastW(t *testing.T) {
	const quarter = 15 * time.Minute
	tests := []struct {
		name                          string
		measured, inflight, forecastW float64
		dt                            time.Duration
		want                          float64
	}{
		{"in-flight slot on 8 Nov", 1390, 3814, 3814, 5 * time.Minute, 1390},
		{"midpoint already passed", 1390, 3814, 3814, -2 * time.Minute, 1390},
		{"next quarter on 8 Nov", 1390, 3814, 3875, 3 * quarter / 2, 1451},
		{"halfway down the fade", 1000, 3000, 3000, 75 * time.Minute, 2000},
		{"end of the fade", 1000, 3000, 3000, 2 * time.Hour, 3000},
		{"beyond the fade", 1000, 3000, 3000, 3 * time.Hour, 3000},
		{"rising measured level", 3000, 1000, 1000, 10 * time.Minute, 3000},
		{"forecast already above the level", 3440, 2060, 3862, 20 * time.Minute, 3862},
		{"forecast already below the level", 1000, 3000, 900, 20 * time.Minute, 900},
		{"never below the measured level", 1000, 3000, 1500, 10 * time.Minute, 1000},
		{"never above the measured level", 5000, 1000, 4500, 10 * time.Minute, 5000},
		{"no correction when on target", 2000, 2000, 2300, 10 * time.Minute, 2300},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NowcastW(tc.measured, tc.inflight, tc.forecastW, tc.dt)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
