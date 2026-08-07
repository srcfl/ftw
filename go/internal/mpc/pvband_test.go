package mpc

import (
	"math"
	"testing"
)

// The optimizer rejects any request where the band fails to bracket the base
// forecast or where either bound crosses zero. This is the invariant the whole
// helper exists to guarantee, so it is asserted over a spread of inputs
// including the degenerate ones.
func TestPVBandHoldsSiteSignInvariant(t *testing.T) {
	bases := []float64{0, -1, -250, -2000, -9500, math.NaN(), math.Inf(-1)}
	spreads := []float64{0, 1, 250, 3000, 100000, -50, math.NaN()}

	for _, base := range bases {
		for _, spread := range spreads {
			low, high := PVBand(base, spread)

			if math.IsNaN(low) || math.IsNaN(high) {
				t.Fatalf("PVBand(%v, %v) produced NaN: low=%v high=%v", base, spread, low, high)
			}
			if low > 0 || high > 0 {
				t.Errorf("PVBand(%v, %v) = (%v, %v); bounds must stay <= 0 (site convention)",
					base, spread, low, high)
			}
			if low > high {
				t.Errorf("PVBand(%v, %v) = (%v, %v); low must not exceed high", base, spread, low, high)
			}
			// The base is only bracketed when it is itself a valid generation
			// figure; invalid bases are normalised to a zero-width band.
			if base < 0 && !math.IsInf(base, 0) {
				if low > base || base > high {
					t.Errorf("PVBand(%v, %v) = (%v, %v); band must bracket the base forecast",
						base, spread, low, high)
				}
			}
		}
	}
}

// A site with no measured uncertainty must plan against the plain forecast --
// the band collapses to a point rather than quietly widening.
func TestPVBandZeroSpreadCollapsesOntoBase(t *testing.T) {
	const base = -3200.0
	low, high := PVBand(base, 0)
	if low != base || high != base {
		t.Errorf("PVBand(%v, 0) = (%v, %v), want both == %v", base, low, high, base)
	}
}

// The clamp that keeps the pessimistic bound from crossing into positive
// territory: a spread wider than the expected generation means "might produce
// nothing", never "might consume".
func TestPVBandClampsPessimisticBoundAtZero(t *testing.T) {
	low, high := PVBand(-1000, 4000)
	if high != 0 {
		t.Errorf("high = %v, want 0 (clamped); a positive bound would read as load", high)
	}
	if low != -5000 {
		t.Errorf("low = %v, want -5000 (optimistic bound is unclamped)", low)
	}
}

// Direction check stated in production terms, so a future sign flip fails here
// with an obvious message rather than as an opaque optimizer ProtocolError.
func TestPVBandLowIsOptimisticHighIsPessimistic(t *testing.T) {
	const base = -5000.0
	low, high := PVBand(base, 1500)
	if generation := -low; generation != 6500 {
		t.Errorf("optimistic generation = %v W, want 6500 W", generation)
	}
	if generation := -high; generation != 3500 {
		t.Errorf("pessimistic generation = %v W, want 3500 W", generation)
	}
}

// Night slots carry no expected generation, so they get no band at all.
func TestPVBandNoGenerationYieldsNoBand(t *testing.T) {
	for _, base := range []float64{0, 250} {
		low, high := PVBand(base, 900)
		if low != 0 || high != 0 {
			t.Errorf("PVBand(%v, 900) = (%v, %v), want (0, 0)", base, low, high)
		}
	}
}
