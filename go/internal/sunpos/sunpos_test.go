package sunpos

import (
	"math"
	"testing"
	"time"
)

// Stockholm noon-ish around summer solstice — sun should be roughly south,
// well above horizon (zenith well below 90° at lat 59).
func TestNoonSummerStockholm(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC) // ~13:00 local
	p := At(tt, 59.33, 18.07)
	if p.ZenithDeg > 50 || p.ZenithDeg < 30 {
		t.Errorf("noon zenith should be ~36°, got %.1f", p.ZenithDeg)
	}
	if p.AzimuthDeg < 150 || p.AzimuthDeg > 210 {
		t.Errorf("noon azimuth should be near south (180°), got %.1f", p.AzimuthDeg)
	}
}

// Night → zenith ≥ 90.
func TestMidnightBelow(t *testing.T) {
	tt := time.Date(2026, 12, 21, 23, 0, 0, 0, time.UTC) // ~midnight local
	p := At(tt, 59.33, 18.07)
	if p.ZenithDeg < 90 {
		t.Errorf("expected sun below horizon, got zenith %.1f", p.ZenithDeg)
	}
	if cs := ClearSkyW(tt, 59.33, 18.07); cs != 0 {
		t.Errorf("clearsky at midnight should be 0, got %.1f", cs)
	}
}

// AOI: south-facing 30° panel at solar noon → low AOI (sun roughly normal).
func TestAOISouthAtNoon(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	sun := At(tt, 59.33, 18.07)
	a := AOI(sun, 30, 180) // south-facing 30° tilt
	if a > 30 {
		t.Errorf("AOI should be small near solar noon, got %.1f", a)
	}
}

// East-facing panel: morning AOI < afternoon AOI (sun in east in morning).
func TestAOIEastVsAfternoon(t *testing.T) {
	morning := time.Date(2026, 6, 21, 5, 0, 0, 0, time.UTC) // ~07 local
	evening := time.Date(2026, 6, 21, 17, 0, 0, 0, time.UTC) // ~19 local
	sunM := At(morning, 59.33, 18.07)
	sunE := At(evening, 59.33, 18.07)
	if sunM.ZenithDeg >= 90 || sunE.ZenithDeg >= 90 {
		t.Skip("sun below horizon — test invalid for this date/place")
	}
	aM := AOI(sunM, 30, 90)  // east-facing
	aE := AOI(sunE, 30, 90)
	if aM >= aE {
		t.Errorf("east panel should see lower AOI in morning (%.1f) than evening (%.1f)", aM, aE)
	}
}

// POA on flat ground = clear-sky horizontal irradiance (within rounding).
func TestPOAFlatEqualsGHI(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	ghi := ClearSkyW(tt, 59.33, 18.07)
	poa := POA(tt, 59.33, 18.07, 0, 180) // tilt 0 = flat
	if math.Abs(ghi-poa) > 1.0 {
		t.Errorf("flat POA should equal GHI: ghi=%.1f poa=%.1f", ghi, poa)
	}
}

// A flat panel receives all of GHI regardless of the diffuse split, because
// beam-on-horizontal + diffuse-on-horizontal reconstructs GHI exactly.
func TestPOAFromComponentsFlatEqualsGHI(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	sun := At(tt, 59.33, 18.07)
	const ghi = 700.0
	for _, dhi := range []float64{0, 140, 350, 700} {
		poa := POAFromComponents(sun, ghi, dhi, 0, 180)
		if math.Abs(poa-ghi) > 0.5 {
			t.Errorf("flat POA should equal GHI for dhi=%.0f: got %.2f want %.0f", dhi, poa, ghi)
		}
	}
}

// A south-tilted panel at Stockholm winter noon (low sun) collects more than a
// flat one — the whole point of projecting onto the plane.
func TestPOAFromComponentsSouthTiltBeatsFlatInWinter(t *testing.T) {
	tt := time.Date(2026, 12, 21, 11, 0, 0, 0, time.UTC) // ~noon local, low sun
	sun := At(tt, 59.33, 18.07)
	if sun.ZenithDeg >= 90 {
		t.Skip("sun below horizon")
	}
	const ghi, dhi = 200.0, 60.0
	flat := POAFromComponents(sun, ghi, dhi, 0, 180)
	tilt := POAFromComponents(sun, ghi, dhi, 45, 180) // 45° south
	if tilt <= flat {
		t.Errorf("south-tilted winter POA (%.1f) should beat flat (%.1f)", tilt, flat)
	}
}

// Sun behind the panel → only the diffuse dome contributes (no negative beam).
func TestPOAFromComponentsSunBehindPanelIsDiffuseOnly(t *testing.T) {
	tt := time.Date(2026, 6, 21, 5, 0, 0, 0, time.UTC) // morning, sun in the east
	sun := At(tt, 59.33, 18.07)
	if sun.ZenithDeg >= 90 {
		t.Skip("sun below horizon")
	}
	const ghi, dhi = 300.0, 90.0
	// A steep west-facing wall can't see the eastern morning sun's beam.
	poa := POAFromComponents(sun, ghi, dhi, 90, 270)
	diffuseOnly := dhi * (1 + math.Cos(90*math.Pi/180)) / 2
	if math.Abs(poa-diffuseOnly) > 0.5 {
		t.Errorf("sun-behind panel should be diffuse-only %.2f, got %.2f", diffuseOnly, poa)
	}
}

// Erbs diffuse fraction: overcast → ~all diffuse, clear → floor at 0.165,
// monotonically non-increasing across the mid range.
func TestErbsDiffuseFraction(t *testing.T) {
	if f := ErbsDiffuseFraction(0); f != 1 {
		t.Errorf("kt=0 → fraction 1, got %.3f", f)
	}
	if f := ErbsDiffuseFraction(1.0); math.Abs(f-0.165) > 1e-9 {
		t.Errorf("kt>0.8 → 0.165, got %.3f", f)
	}
	clear := ErbsDiffuseFraction(0.75)
	murky := ErbsDiffuseFraction(0.30)
	if !(clear < murky) {
		t.Errorf("clearer sky should have less diffuse: clear=%.3f murky=%.3f", clear, murky)
	}
	if clear < 0.165 || clear > 1 {
		t.Errorf("fraction out of [0.165,1]: %.3f", clear)
	}
}

// POAFromGHI (Erbs split) on a flat panel still reconstructs ~GHI.
func TestPOAFromGHIFlatApproxGHI(t *testing.T) {
	tt := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	const ghi = 600.0
	poa := POAFromGHI(tt, 59.33, 18.07, ghi, 0, 180)
	if math.Abs(poa-ghi) > 0.5 {
		t.Errorf("flat POAFromGHI should equal GHI: got %.2f want %.0f", poa, ghi)
	}
}

func TestPOAFromGHINonFiniteOrNonPositiveGHIIsZero(t *testing.T) {
	when := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		ghi  float64
	}{
		{name: "NaN", ghi: math.NaN()},
		{name: "positive infinity", ghi: math.Inf(1)},
		{name: "negative infinity", ghi: math.Inf(-1)},
		{name: "negative", ghi: -1},
		{name: "zero", ghi: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := POAFromGHI(when, 59.33, 18.07, tc.ghi, 35, 180); got != 0 {
				t.Errorf("POAFromGHI(%v) = %v, want 0", tc.ghi, got)
			}
		})
	}
}

// Night → zero from both measured-irradiance variants regardless of input.
func TestPOAVariantsZeroAtNight(t *testing.T) {
	tt := time.Date(2026, 12, 21, 23, 0, 0, 0, time.UTC)
	sun := At(tt, 59.33, 18.07)
	if p := POAFromComponents(sun, 500, 100, 35, 180); p != 0 {
		t.Errorf("night POAFromComponents should be 0, got %.2f", p)
	}
	if p := POAFromGHI(tt, 59.33, 18.07, 500, 35, 180); p != 0 {
		t.Errorf("night POAFromGHI should be 0, got %.2f", p)
	}
}
