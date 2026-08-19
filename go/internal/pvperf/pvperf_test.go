package pvperf

import (
	"math"
	"testing"
	"time"
)

// A few clear-ish midday summer hours at Stockholm, GHI + DHI populated.
func summerHours() []Irradiance {
	base := time.Date(2024, 6, 21, 8, 0, 0, 0, time.UTC)
	ghi := []float64{300, 450, 600, 700, 680, 550, 400, 250}
	out := make([]Irradiance, len(ghi))
	for i, g := range ghi {
		d := g * 0.25
		out[i] = Irradiance{HourStart: base.Add(time.Duration(i) * time.Hour), GHIWm2: g, DHIWm2: &d}
	}
	return out
}

func TestExpectedWhScalesWithRatedW(t *testing.T) {
	hours := summerHours()
	e5 := ExpectedWh(59.33, 18.07, []Array{{RatedW: 5000, TiltDeg: 35, AzimuthDeg: 180}}, hours)
	e10 := ExpectedWh(59.33, 18.07, []Array{{RatedW: 10000, TiltDeg: 35, AzimuthDeg: 180}}, hours)
	if e5 <= 0 {
		t.Fatalf("expected positive energy, got %.1f", e5)
	}
	if math.Abs(e10/e5-2) > 1e-9 {
		t.Errorf("10 kW should be 2× 5 kW, ratio %.4f", e10/e5)
	}
}

func TestExpectedWhSumsArrays(t *testing.T) {
	hours := summerHours()
	south := ExpectedWh(59.33, 18.07, []Array{{RatedW: 5000, TiltDeg: 35, AzimuthDeg: 180}}, hours)
	east := ExpectedWh(59.33, 18.07, []Array{{RatedW: 5000, TiltDeg: 35, AzimuthDeg: 90}}, hours)
	both := ExpectedWh(59.33, 18.07, []Array{
		{RatedW: 5000, TiltDeg: 35, AzimuthDeg: 180},
		{RatedW: 5000, TiltDeg: 35, AzimuthDeg: 90},
	}, hours)
	if math.Abs(both-(south+east)) > 1e-6 {
		t.Errorf("multi-array should sum: both=%.2f south+east=%.2f", both, south+east)
	}
}

// Measured diffuse should be honoured: an all-diffuse hour yields less on a
// south-tilted panel than the GHI-only Erbs path (which infers more beam).
func TestExpectedWhUsesMeasuredDiffuse(t *testing.T) {
	base := time.Date(2024, 6, 21, 11, 0, 0, 0, time.UTC)
	ghiOnly := []Irradiance{{HourStart: base, GHIWm2: 600}}
	full := 600.0
	allDiffuse := []Irradiance{{HourStart: base, GHIWm2: 600, DHIWm2: &full}}
	arr := []Array{{RatedW: 10000, TiltDeg: 35, AzimuthDeg: 180}}

	eErbs := ExpectedWh(59.33, 18.07, arr, ghiOnly)
	eDiffuse := ExpectedWh(59.33, 18.07, arr, allDiffuse)
	if !(eDiffuse < eErbs) {
		t.Errorf("all-diffuse (%.1f) should be < Erbs-inferred-beam (%.1f)", eDiffuse, eErbs)
	}
}

func TestExpectedWhZeroWhenDark(t *testing.T) {
	night := []Irradiance{{HourStart: time.Date(2024, 12, 21, 23, 0, 0, 0, time.UTC), GHIWm2: 0}}
	if e := ExpectedWh(59.33, 18.07, []Array{{RatedW: 10000, TiltDeg: 35, AzimuthDeg: 180}}, night); e != 0 {
		t.Errorf("dark hours should yield 0 Wh, got %.2f", e)
	}
}

func TestPerformanceRatio(t *testing.T) {
	if _, ok := PerformanceRatio(50, 40); ok {
		t.Error("tiny expected should be n/a")
	}
	pr, ok := PerformanceRatio(1000, 850)
	if !ok || math.Abs(pr-0.85) > 1e-9 {
		t.Errorf("pr=%.3f ok=%v, want 0.85 true", pr, ok)
	}
	if pr, _ := PerformanceRatio(1000, 5000); pr != 2 {
		t.Errorf("PR should clamp high to 2, got %.2f", pr)
	}
	if pr, _ := PerformanceRatio(1000, -10); pr != 0 {
		t.Errorf("PR should clamp low to 0, got %.2f", pr)
	}
}
