package control

import (
	"testing"
	"time"
)

// runHoldoffTicks models main.go: ComputeDispatch runs once per fixed 2 s
// ticker tick, after a variable amount of pre-dispatch work. Each clock read
// inside ComputeDispatch advances the fake clock by 50 µs to model the
// dispatch call's own run time. It returns how many ticks sent no battery
// command.
func runHoldoffTicks(t *testing.T, minDispatchS, controlS int, preWorkMs []int) (skipped int) {
	t.Helper()
	t0 := time.Now()
	st := NewState(0, 42, "meter")
	st.MinDispatchIntervalS = minDispatchS
	st.ControlIntervalS = controlS
	st.SlewRateW = 3000
	var now time.Time
	st.clock = func() time.Time {
		now = now.Add(50 * time.Microsecond)
		return now
	}
	capsMap := caps(map[string]float64{"bat": 10000})
	for k, w := range preWorkMs {
		// Ticker fires at t0 + 2k s; pre-dispatch work takes w ms.
		now = t0.Add(time.Duration(2*k)*time.Second + time.Duration(w)*time.Millisecond)
		// House imports 1500 W, battery idle: every normal tick must discharge.
		store := seedDeadbandSite(1500, []deadbandBattery{{"bat", 0, 0.6, ""}})
		if out := ComputeDispatch(store, st, capsMap, 0); len(out) == 0 {
			skipped++
			t.Logf("tick %d (+%d ms): holdoff exit, no battery command", k, w)
		}
	}
	return skipped
}

// Constant pre-dispatch work: no jitter at all. Before the tolerance, the
// dispatch call's own run time alone pulled elapsed just below 2 s on every
// tick after a dispatch, so half of all ticks sent no battery command.
func TestHoldoffDefaultCadenceConstantWork(t *testing.T) {
	work := []int{100, 100, 100, 100, 100, 100, 100, 100, 100, 100}
	if n := runHoldoffTicks(t, 2, 0, work); n > 0 {
		t.Errorf("%d of %d ticks issued no battery command at the default 2 s/2 s cadence", n, len(work))
	}
}

// Jittered pre-dispatch work, as from loadpoint commands and SQL writes.
func TestHoldoffDefaultCadenceJitteredWork(t *testing.T) {
	work := []int{120, 80, 150, 90, 140, 60, 110, 100, 130, 70}
	if n := runHoldoffTicks(t, 2, 0, work); n > 0 {
		t.Errorf("%d of %d ticks issued no battery command at the default 2 s/2 s cadence", n, len(work))
	}
}

// A charger command can hold one tick's pre-work for up to half the control
// interval. With the holdoff no longer than one tick, no tick is skipped,
// even when the jitter is far beyond the tolerance.
func TestHoldoffOffWhenNotLongerThanControlInterval(t *testing.T) {
	work := []int{1000, 50, 900, 20, 1000, 10, 800, 30, 1000, 0}
	if n := runHoldoffTicks(t, 2, 2, work); n > 0 {
		t.Errorf("%d of %d ticks issued no battery command with min_dispatch_interval_s == control_interval_s", n, len(work))
	}
}

// A holdoff longer than the tick still spaces the commands: 5 s on 2 s
// ticks dispatches at 0, 6, 12 and 18 s and holds the six ticks between.
func TestHoldoffLongerThanControlIntervalStillHolds(t *testing.T) {
	work := []int{100, 100, 100, 100, 100, 100, 100, 100, 100, 100}
	if n := runHoldoffTicks(t, 5, 2, work); n != 6 {
		t.Errorf("got %d held ticks, want 6 with a 5 s holdoff on 2 s ticks", n)
	}
}
