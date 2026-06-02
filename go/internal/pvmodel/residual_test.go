package pvmodel

import (
	"math"
	"testing"
	"time"
)

// Anchor for deterministic test time math. UTC daylight afternoon so any
// downstream "must be daylight" gates don't accidentally short-circuit.
var residualNow = time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)

// ResidualStdW feeds the MPC's downside-PV safety (σ). It must surface the
// residual-buffer std, and degrade to 0 (no hedge) when there's no buffer.
func TestServiceResidualStdW(t *testing.T) {
	if got := (*Service)(nil).ResidualStdW(); got != 0 {
		t.Errorf("nil Service → 0, got %v", got)
	}
	if got := (&Service{}).ResidualStdW(); got != 0 {
		t.Errorf("nil Residuals buffer → 0, got %v", got)
	}
	b := NewResidualBuffer()
	now := time.Now()
	for i := 0; i < 10; i++ {
		ts := now.Add(time.Duration(-45+i*5) * time.Minute) // 45..0 min ago, inside the 2h window
		b.Add(ts, 1000, float64(700+i*60))                  // residuals span −300..+240 → positive std
	}
	want := b.Diag(time.Now()).StdW
	if want <= 0 {
		t.Fatalf("setup: varying residuals must yield a positive std, got %v", want)
	}
	if got := (&Service{Residuals: b}).ResidualStdW(); math.Abs(got-want) > 1e-9 {
		t.Errorf("ResidualStdW = %v, want Diag().StdW = %v", got, want)
	}
}

// TestResidualBufferAdd_AppendsAndAgesOff: samples older than the buffer
// window relative to the most-recently-added sample must be dropped.
func TestResidualBufferAdd_AppendsAndAgesOff(t *testing.T) {
	b := NewResidualBuffer()
	// Add 30 samples spanning 3 hours, every 6 min, ending at residualNow.
	// Window is 2h → only samples within last 120 min should remain.
	for i := 0; i < 30; i++ {
		ts := residualNow.Add(time.Duration(-29+i) * 6 * time.Minute)
		b.Add(ts, 1000, 800)
	}
	// Cutoff: most recent is at residualNow. Window = 2h. Anything with
	// t < residualNow - 2h is aged off.
	cutoff := residualNow.Add(-2 * time.Hour)
	got := b.Len()
	// Count expected remaining: indices where ts >= cutoff.
	expected := 0
	for i := 0; i < 30; i++ {
		ts := residualNow.Add(time.Duration(-29+i) * 6 * time.Minute)
		if !ts.Before(cutoff) {
			expected++
		}
	}
	if got != expected {
		t.Fatalf("after age-off: got %d samples, want %d", got, expected)
	}
	if expected >= 30 {
		t.Fatalf("test invariant: expected age-off to drop something, expected=%d", expected)
	}
}

// TestResidualBufferAdd_CapsAtMaxSamples: even within window, the buffer
// never grows past MaxSamples; oldest drop first.
func TestResidualBufferAdd_CapsAtMaxSamples(t *testing.T) {
	b := NewResidualBuffer()
	// Window 2h, MaxSamples 240. Add 500 samples at 1s cadence all within
	// the last 2h — buffer must cap at 240, retaining the newest.
	start := residualNow.Add(-500 * time.Second)
	for i := 0; i < 500; i++ {
		ts := start.Add(time.Duration(i) * time.Second)
		b.Add(ts, float64(i), float64(i)+10) // predictable so we can verify newest-kept
	}
	if got := b.Len(); got != residualBufferMaxSamples {
		t.Fatalf("got %d samples, want %d", got, residualBufferMaxSamples)
	}
	// Oldest kept must be sample (500 - 240) = 260; verify via the
	// internal slice (predicted == 260).
	b.mu.Lock()
	oldestPred := b.samples[0].predicted
	newestPred := b.samples[len(b.samples)-1].predicted
	b.mu.Unlock()
	if oldestPred != float64(500-residualBufferMaxSamples) {
		t.Fatalf("oldest kept sample predicted=%v, want %v",
			oldestPred, float64(500-residualBufferMaxSamples))
	}
	if newestPred != 499 {
		t.Fatalf("newest kept sample predicted=%v, want 499", newestPred)
	}
}

// TestResidualBuffer_Correct_ReturnsZeroWhenBelowMinSamples: 10 samples,
// below MinSamples threshold (20) → no correction applied.
func TestResidualBuffer_Correct_ReturnsZeroWhenBelowMinSamples(t *testing.T) {
	b := NewResidualBuffer()
	for i := 0; i < 10; i++ {
		ts := residualNow.Add(time.Duration(-i) * time.Minute)
		b.Add(ts, 1000, 1500) // big bias, but below MinSamples
	}
	target := residualNow.Add(15 * time.Minute)
	got := b.Correct(residualNow, target, 1000)
	if got != 0 {
		t.Fatalf("below MinSamples: got correction %v, want 0", got)
	}
}

// TestResidualBuffer_Correct_ReturnsZeroWhenVarianceHigh: bias 100 W but
// std 200 W → noise dominates signal → no correction.
func TestResidualBuffer_Correct_ReturnsZeroWhenVarianceHigh(t *testing.T) {
	b := NewResidualBuffer()
	// 30 samples with mean residual ~100, std ~200.
	// Generate by alternating +300 and -100 → mean +100, std 200.
	resids := []float64{300, -100, 300, -100, 300, -100, 300, -100, 300, -100,
		300, -100, 300, -100, 300, -100, 300, -100, 300, -100,
		300, -100, 300, -100, 300, -100, 300, -100, 300, -100}
	for i, r := range resids {
		ts := residualNow.Add(time.Duration(-i) * time.Minute)
		// actual - predicted = r ⇒ pick predicted=1000, actual=1000+r.
		b.Add(ts, 1000, 1000+r)
	}
	target := residualNow.Add(15 * time.Minute)
	got := b.Correct(residualNow, target, 1000)
	if got != 0 {
		t.Fatalf("high-variance: got correction %v, want 0", got)
	}
}

// TestResidualBuffer_Correct_AppliesConstantBias: 30 samples with mean
// residual −200 W (actual generates 200 W MORE than predicted, recall
// site-sign: more generation = more negative), std ~30 → return ~ −200
// at full ramp-on.
func TestResidualBuffer_Correct_AppliesConstantBias(t *testing.T) {
	b := NewResidualBuffer()
	// Small zigzag around −200 to keep std ~30.
	for i := 0; i < 30; i++ {
		ts := residualNow.Add(time.Duration(-i) * time.Minute)
		jitter := -30.0
		if i%2 == 0 {
			jitter = 30.0
		}
		// actual - predicted = -200 + jitter
		b.Add(ts, 1000, 1000+(-200+jitter))
	}
	target := residualNow.Add(15 * time.Minute) // dt < 30min → factor = 1
	got := b.Correct(residualNow, target, 1000)
	if math.Abs(got-(-200)) > 5 {
		t.Fatalf("constant bias: got %v, want ~-200", got)
	}
}

// addNiceBias seeds the buffer with 30 samples, mean residual -200 W
// and very low std. Used for ramp-off tests.
func addNiceBias(b *ResidualBuffer) {
	for i := 0; i < 30; i++ {
		ts := residualNow.Add(time.Duration(-i) * time.Minute)
		// tiny jitter so std isn't exactly zero (we still want sigma>0
		// but well under |mean|).
		jitter := -2.0
		if i%2 == 0 {
			jitter = 2.0
		}
		b.Add(ts, 1000, 1000+(-200+jitter))
	}
}

func TestResidualBuffer_Correct_RampOff_30min(t *testing.T) {
	b := NewResidualBuffer()
	addNiceBias(b)
	target := residualNow.Add(30 * time.Minute)
	got := b.Correct(residualNow, target, 1000)
	// At dt=30 min, factor = 1 (boundary inclusive); expect ~ -200.
	if math.Abs(got-(-200)) > 5 {
		t.Fatalf("dt=30min: got %v, want ~-200 (factor=1)", got)
	}
}

func TestResidualBuffer_Correct_RampOff_75min(t *testing.T) {
	b := NewResidualBuffer()
	addNiceBias(b)
	target := residualNow.Add(75 * time.Minute)
	got := b.Correct(residualNow, target, 1000)
	// At dt=75 min, factor = 1 - (75-30)/90 = 0.5; expect ~ -100.
	if math.Abs(got-(-100)) > 5 {
		t.Fatalf("dt=75min: got %v, want ~-100 (factor=0.5)", got)
	}
}

func TestResidualBuffer_Correct_RampOff_120min(t *testing.T) {
	b := NewResidualBuffer()
	addNiceBias(b)
	target := residualNow.Add(120 * time.Minute)
	got := b.Correct(residualNow, target, 1000)
	// At dt=120 min, factor = 0.
	if math.Abs(got) > 1e-6 {
		t.Fatalf("dt=120min: got %v, want 0 (factor=0)", got)
	}
}

func TestResidualBuffer_Correct_RampOff_Beyond2h(t *testing.T) {
	b := NewResidualBuffer()
	addNiceBias(b)
	target := residualNow.Add(180 * time.Minute)
	got := b.Correct(residualNow, target, 1000)
	if math.Abs(got) > 1e-6 {
		t.Fatalf("dt=180min: got %v, want 0", got)
	}
}

// Past targets (dt < 0) must not be corrected either.
func TestResidualBuffer_Correct_PastTargetIsZero(t *testing.T) {
	b := NewResidualBuffer()
	addNiceBias(b)
	target := residualNow.Add(-10 * time.Minute)
	got := b.Correct(residualNow, target, 1000)
	if got != 0 {
		t.Fatalf("dt<0: got %v, want 0", got)
	}
}

// Tiny mean (< epsilon) should be gated to zero.
func TestResidualBuffer_Correct_TinyMeanGated(t *testing.T) {
	b := NewResidualBuffer()
	// mean residual 10 W (below 25 W epsilon), tiny std.
	for i := 0; i < 30; i++ {
		ts := residualNow.Add(time.Duration(-i) * time.Minute)
		jitter := -1.0
		if i%2 == 0 {
			jitter = 1.0
		}
		b.Add(ts, 1000, 1000+(10+jitter))
	}
	target := residualNow.Add(15 * time.Minute)
	got := b.Correct(residualNow, target, 1000)
	if got != 0 {
		t.Fatalf("tiny mean: got %v, want 0", got)
	}
}

// Diagnostic helpers expose the current state for /api/pvmodel.
func TestResidualBuffer_Diag_SamplesAndStd(t *testing.T) {
	b := NewResidualBuffer()
	addNiceBias(b)
	d := b.Diag(residualNow)
	if d.SampleCount != 30 {
		t.Fatalf("SampleCount=%d, want 30", d.SampleCount)
	}
	if d.StdW <= 0 {
		t.Fatalf("StdW=%v, want >0", d.StdW)
	}
	if math.Abs(d.MeanW-(-200)) > 5 {
		t.Fatalf("MeanW=%v, want ~-200", d.MeanW)
	}
}
