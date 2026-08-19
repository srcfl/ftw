package pvperf

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// scoredDays builds a run of days carrying the given performance ratios.
func scoredDays(ratios ...float64) []state.PVPerformanceDay {
	days := make([]state.PVPerformanceDay, 0, len(ratios))
	for i, r := range ratios {
		ratio := r
		days = append(days, state.PVPerformanceDay{
			Day:        fmt.Sprintf("2026-07-%02d", i+1),
			ExpectedWh: 50000,
			ActualWh:   50000 * ratio,
			PR:         &ratio,
		})
	}
	return days
}

// A handful of days is not evidence. Applying a factor from a thin sample would
// let one cloudy week permanently depress the forecast.
func TestCalibrateNeedsEnoughDays(t *testing.T) {
	c := Calibrate(scoredDays(0.9, 0.9, 0.9))
	if c.Valid {
		t.Error("calibration should not be valid on 3 days")
	}
	if c.Days != 3 {
		t.Errorf("Days = %d, want 3", c.Days)
	}
}

// Days without a usable ratio (polar night, no history) must not be counted as
// evidence, and must not drag the factor toward zero.
func TestCalibrateIgnoresDaysWithoutRatio(t *testing.T) {
	days := scoredDays(0.9, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9)
	days = append(days, state.PVPerformanceDay{Day: "2026-07-30", ExpectedWh: 10, ActualWh: 0})
	c := Calibrate(days)
	if c.Days != 7 {
		t.Errorf("Days = %d, want 7 (the ratio-less day is not evidence)", c.Days)
	}
	if math.Abs(c.Factor-0.9) > 1e-9 {
		t.Errorf("Factor = %v, want 0.9", c.Factor)
	}
}

// The reason for median-over-mean: a snow day is a huge outlier, and a site
// that normally runs at 0.90 must still calibrate to ~0.90 despite one.
func TestCalibrateResistsOutlierDays(t *testing.T) {
	c := Calibrate(scoredDays(0.90, 0.91, 0.89, 0.90, 0.92, 0.88, 0.90, 0.05))
	if !c.Valid {
		t.Fatal("expected a valid calibration")
	}
	if math.Abs(c.Factor-0.90) > 0.02 {
		t.Errorf("Factor = %v, want ~0.90; a single snow day should not move it", c.Factor)
	}
}

// A factor this low means the geometry or the meter is misconfigured. Silently
// rescaling the forecast would bake the fault in instead of surfacing it.
func TestCalibrateRejectsImplausiblyLowFactor(t *testing.T) {
	c := Calibrate(scoredDays(0.10, 0.11, 0.09, 0.10, 0.12, 0.08, 0.10, 0.11))
	if c.Valid {
		t.Errorf("factor %v should be rejected as implausible", c.Factor)
	}
	if c.Days != 8 {
		t.Errorf("Days = %d, want 8 — the sample is still reported", c.Days)
	}
}

// Likewise for a site consistently beating its own nameplate: that is an
// understated kWp, not a bonus to hand to the planner.
func TestCalibrateRejectsImplausiblyHighFactor(t *testing.T) {
	c := Calibrate(scoredDays(1.6, 1.7, 1.55, 1.62, 1.58, 1.7, 1.65, 1.6))
	if c.Valid {
		t.Errorf("factor %v should be rejected as implausible", c.Factor)
	}
}

// A perfectly steady site reports no uncertainty, so the planner's band
// collapses onto the forecast rather than inventing spread.
func TestCalibrateSigmaIsZeroForSteadySite(t *testing.T) {
	c := Calibrate(scoredDays(0.9, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9, 0.9))
	if !c.Valid {
		t.Fatal("expected a valid calibration")
	}
	if c.SigmaRel != 0 {
		t.Errorf("SigmaRel = %v, want 0 for an unvarying site", c.SigmaRel)
	}
}

// A variable site reports a real spread, which is what makes the measured band
// better than a hand-tuned constant.
func TestCalibrateSigmaGrowsWithSpread(t *testing.T) {
	steady := Calibrate(scoredDays(0.90, 0.91, 0.89, 0.90, 0.91, 0.89, 0.90, 0.90))
	variable := Calibrate(scoredDays(0.60, 0.95, 0.70, 0.99, 0.65, 0.92, 0.75, 0.88))
	if !(variable.SigmaRel > steady.SigmaRel) {
		t.Errorf("variable SigmaRel %v should exceed steady %v", variable.SigmaRel, steady.SigmaRel)
	}
}

// The forecast hook is wired unconditionally at startup, so it must be safe on
// a service that was never built.
func TestCalibrationFactorNilServiceIsSafe(t *testing.T) {
	var s *Service
	factor, ok := s.CalibrationFactor()
	if ok {
		t.Errorf("nil service reported a usable factor (%v)", factor)
	}
}

// End-to-end through the store, since that is how the API and the forecast
// hook actually reach it.
func TestServiceCalibrationReadsPersistedDays(t *testing.T) {
	st := openStore(t)
	svc := &Service{Store: st}

	if c := svc.Calibration(); c.Valid || c.Days != 0 {
		t.Fatalf("empty store should yield no calibration, got %+v", c)
	}

	now := time.Now()
	for i := 1; i <= 10; i++ {
		ratio := 0.88
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		if err := st.SavePVPerformance(state.PVPerformanceDay{
			Day: day, ExpectedWh: 40000, ActualWh: 40000 * ratio, PR: &ratio,
		}); err != nil {
			t.Fatalf("save %s: %v", day, err)
		}
	}
	c := svc.Calibration()
	if !c.Valid {
		t.Fatalf("expected a valid calibration, got %+v", c)
	}
	if math.Abs(c.Factor-0.88) > 1e-9 {
		t.Errorf("Factor = %v, want 0.88", c.Factor)
	}
	if c.Days != 10 {
		t.Errorf("Days = %d, want 10", c.Days)
	}

	factor, ok := svc.CalibrationFactor()
	if !ok || math.Abs(factor-0.88) > 1e-9 {
		t.Errorf("CalibrationFactor() = (%v, %v), want (0.88, true)", factor, ok)
	}
}
