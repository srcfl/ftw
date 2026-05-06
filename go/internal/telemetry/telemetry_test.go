package telemetry

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// ---- Kalman ----

func TestKalmanInitialMeasurement(t *testing.T) {
	k := NewKalman(100, 50)
	out := k.Update(1234)
	if out != 1234 {
		t.Errorf("first update should return measurement, got %f", out)
	}
	if k.Estimate != 1234 {
		t.Errorf("estimate: got %f", k.Estimate)
	}
}

func TestKalmanSmoothsStepChange(t *testing.T) {
	k := NewKalman(100, 50)
	k.Update(1000)
	out := k.Update(2000)
	if out <= 1000 || out >= 2000 {
		t.Errorf("expected smoothed value between 1000 and 2000, got %f", out)
	}
}

func TestKalmanConvergesOnStableSignal(t *testing.T) {
	k := NewKalman(100, 50)
	for i := 0; i < 50; i++ {
		k.Update(500)
	}
	if math.Abs(k.Estimate-500) > 1 {
		t.Errorf("expected ≈500 after convergence, got %f", k.Estimate)
	}
}

// ---- Store ----

func TestStoreUpdateAndGet(t *testing.T) {
	s := NewStore()
	s.Update("ferroamp", DerMeter, 1500, nil, nil)
	r := s.Get("ferroamp", DerMeter)
	if r == nil {
		t.Fatal("expected reading")
	}
	if r.RawW != 1500 {
		t.Errorf("raw: got %f", r.RawW)
	}
	if r.SmoothedW != 1500 {
		t.Errorf("first smoothed = raw, got %f", r.SmoothedW)
	}
}

func TestStoreKeepsSeparateFiltersPerDriver(t *testing.T) {
	s := NewStore()
	soc := 0.5
	s.Update("a", DerBattery, 100, &soc, nil)
	s.Update("b", DerBattery, 200, &soc, nil)
	s.Update("a", DerPV, 300, nil, nil)

	if r := s.Get("a", DerBattery); r == nil || r.RawW != 100 {
		t.Error("a:battery")
	}
	if r := s.Get("b", DerBattery); r == nil || r.RawW != 200 {
		t.Error("b:battery")
	}
	if r := s.Get("a", DerPV); r == nil || r.RawW != 300 {
		t.Error("a:pv")
	}
	if r := s.Get("b", DerPV); r != nil {
		t.Error("b:pv should not exist")
	}
}

func TestReadingsByTypeAggregates(t *testing.T) {
	s := NewStore()
	s.Update("a", DerBattery, 100, nil, nil)
	s.Update("b", DerBattery, 200, nil, nil)
	s.Update("a", DerMeter, 50, nil, nil)

	bats := s.ReadingsByType(DerBattery)
	if len(bats) != 2 {
		t.Errorf("expected 2 batteries, got %d", len(bats))
	}
	var total float64
	for _, b := range bats {
		total += b.RawW
	}
	if total != 300 {
		t.Errorf("total battery: got %f", total)
	}
}

func TestIsStale(t *testing.T) {
	s := NewStore()
	if !s.IsStale("unknown", DerMeter, time.Second) {
		t.Error("unknown should be stale")
	}
	s.Update("a", DerMeter, 0, nil, nil)
	if s.IsStale("a", DerMeter, time.Second) {
		t.Error("just-updated should be fresh")
	}
}

// ---- DriverHealth ----

func TestHealthDegradesAfter3Errors(t *testing.T) {
	h := &DriverHealth{Name: "t"}
	h.RecordError("e1")
	if h.Status != StatusOk {
		t.Errorf("after 1 error: %v", h.Status)
	}
	h.RecordError("e2")
	h.RecordError("e3")
	if h.Status != StatusDegraded {
		t.Errorf("after 3 errors should be Degraded, got %v", h.Status)
	}
}

func TestHealthRecoversOnSuccess(t *testing.T) {
	h := &DriverHealth{Name: "t"}
	h.RecordError("e1"); h.RecordError("e2"); h.RecordError("e3")
	h.RecordSuccess()
	if h.Status != StatusOk {
		t.Errorf("success should reset to Ok: %v", h.Status)
	}
	if h.ConsecutiveErrors != 0 {
		t.Errorf("errors should reset: %d", h.ConsecutiveErrors)
	}
}

func TestHealthOffline(t *testing.T) {
	h := &DriverHealth{Name: "t"}
	h.SetOffline()
	if h.IsOnline() {
		t.Error("offline should not be online")
	}
	h.RecordSuccess()
	if !h.IsOnline() {
		t.Error("success should return to online")
	}
}

// ---- Data pass-through ----

func TestReadingPreservesData(t *testing.T) {
	s := NewStore()
	raw := json.RawMessage(`{"hello":"world"}`)
	s.Update("a", DerMeter, 0, nil, raw)
	r := s.Get("a", DerMeter)
	if string(r.Data) != string(raw) {
		t.Errorf("data roundtrip: got %s", string(r.Data))
	}
}

// ---- DerType ----

func TestDerTypeRoundtrip(t *testing.T) {
	for _, name := range []string{"meter", "pv", "battery", "ev"} {
		d, err := ParseDerType(name)
		if err != nil {
			t.Fatal(err)
		}
		if d.String() != name {
			t.Errorf("roundtrip %q: got %q", name, d.String())
		}
	}
	if _, err := ParseDerType("nonsense"); err == nil {
		t.Error("expected parse error")
	}
}

// ---- SoC preservation ----

func TestStorePreservesSoCWhenMissing(t *testing.T) {
	// Devices like Ferroamp ESO publish SoC less often than the
	// power-flow telemetry. In-between ticks have no SoC field — the
	// store must keep the last-known value rather than dropping back
	// to nil, which would confuse the MPC and any UI display.
	s := NewStore()
	soc := 0.97
	s.Update("ferroamp", DerBattery, -1500, &soc, nil)
	if r := s.Get("ferroamp", DerBattery); r == nil || r.SoC == nil || *r.SoC != 0.97 {
		t.Fatalf("first update: SoC not stored, got %+v", r)
	}
	// Next tick: power update only, no SoC.
	s.Update("ferroamp", DerBattery, -1450, nil, nil)
	r := s.Get("ferroamp", DerBattery)
	if r == nil || r.SoC == nil || *r.SoC != 0.97 {
		t.Errorf("SoC should be preserved across nil-update, got %+v", r)
	}
	// Fresh SoC overwrites.
	soc2 := 0.95
	s.Update("ferroamp", DerBattery, -1400, &soc2, nil)
	if r := s.Get("ferroamp", DerBattery); r == nil || r.SoC == nil || *r.SoC != 0.95 {
		t.Errorf("fresh SoC should overwrite, got %+v", r)
	}
}

// ---- Load filter ----

func TestLoadFilterClampsNegative(t *testing.T) {
	// During driver startup or PV/meter sync lag, load = grid - pv - bat
	// can go briefly negative. The filter must clamp to zero so the Kalman
	// doesn't track garbage downward.
	s := NewStore()

	// Seed with a realistic positive load so the filter has state.
	for i := 0; i < 10; i++ {
		s.UpdateLoad(1000)
	}

	// Feed a burst of negative values.
	var last float64
	for i := 0; i < 5; i++ {
		last = s.UpdateLoad(-500)
	}
	if last < 0 {
		t.Errorf("load filter should never return negative after clamping, got %f", last)
	}

	// Single negative value should also be clamped.
	out := s.UpdateLoad(-1)
	if out < 0 {
		t.Errorf("single negative load should be clamped, got %f", out)
	}
}

func TestLoadFilterSmoothsNoisy(t *testing.T) {
	s := NewStore()
	vals := []float64{1000, 2000, 500, 1500, 1200, 800, 1100, 1400}
	var last float64
	for _, v := range vals {
		last = s.UpdateLoad(v)
	}
	if last < 500 || last > 2000 {
		t.Errorf("load filter should converge to middle, got %f", last)
	}
}

// Happy path: two online EV drivers charging at 3.6 kW each → sum = 7200.
func TestSumOnlineEVWSumsAllOnline(t *testing.T) {
	s := NewStore()
	s.Update("easee-1", DerEV, 3600, nil, nil)
	s.Update("easee-2", DerEV, 3600, nil, nil)
	s.DriverHealthMut("easee-1").RecordSuccess()
	s.DriverHealthMut("easee-2").RecordSuccess()
	if got := s.SumOnlineEVW(); got != 7200 {
		t.Errorf("want 7200, got %f", got)
	}
}

// Offline drivers are excluded. Without this, a driver whose watchdog
// tripped would leak a stale last-known reading into load / grid math
// indefinitely after it stopped actually reporting.
func TestSumOnlineEVWSkipsOfflineDrivers(t *testing.T) {
	s := NewStore()
	s.Update("easee-online", DerEV, 3600, nil, nil)
	s.Update("easee-offline", DerEV, 5000, nil, nil)
	s.DriverHealthMut("easee-online").RecordSuccess()
	// Simulate the watchdog flipping easee-offline to offline after a
	// stale telemetry window — its last reading is still in the store
	// but shouldn't count.
	s.DriverHealthMut("easee-offline").SetOffline()
	if got := s.SumOnlineEVW(); got != 3600 {
		t.Errorf("want 3600 (only online), got %f", got)
	}
}

// Non-EV readings (meter, pv, battery) must never leak into the sum —
// otherwise swapping the type enum order would silently change behaviour.
func TestSumOnlineEVWIgnoresOtherDerTypes(t *testing.T) {
	s := NewStore()
	s.Update("meter", DerMeter, 2000, nil, nil)
	s.Update("pv-1", DerPV, -5000, nil, nil)
	s.Update("bat-1", DerBattery, 1000, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	s.DriverHealthMut("pv-1").RecordSuccess()
	s.DriverHealthMut("bat-1").RecordSuccess()
	if got := s.SumOnlineEVW(); got != 0 {
		t.Errorf("no EV readings, want 0, got %f", got)
	}
}

// Empty store → 0, no panic.
func TestSumOnlineEVWEmptyStore(t *testing.T) {
	if got := NewStore().SumOnlineEVW(); got != 0 {
		t.Errorf("want 0, got %f", got)
	}
}

// Regression: a Kalman residual decaying toward zero produces smoothed
// values like 1e-77 (subnormal). Without the sub-watt floor those leak
// into State.EVChargingW via control/dispatch.go's `evSum > 0` guard,
// then trip the BatteryCoversEV cap (dispatch.go:730) which inverts a
// planned discharge into a charge command, which trips applyPlanSignFloor
// and zeros every battery target — observed live as
// `ev_charging_w: 1.09e-77` blocking a peak-price discharge slot.
func TestSumOnlineEVWFloorsSubWattReadings(t *testing.T) {
	s := NewStore()
	s.Update("easee", DerEV, 1e-77, nil, nil)
	s.DriverHealthMut("easee").RecordSuccess()
	if got := s.SumOnlineEVW(); got != 0 {
		t.Errorf("subnormal EV reading must floor to 0, got %g", got)
	}
	// Negative trash (e.g. 1e-300 sign-flipped by another path) must
	// also clamp — any consumer's `> 0` guard is honest only when the
	// floor is symmetric.
	s.Update("easee", DerEV, -0.5, nil, nil)
	if got := s.SumOnlineEVW(); got != 0 {
		t.Errorf("sub-watt negative EV reading must floor to 0, got %g", got)
	}
	// Sanity: a real charger draw on a fresh store passes through (the
	// Kalman seeds itself from the first measurement, so first-update
	// equality is meaningful; subsequent updates are smoothed and not
	// useful as an exact assertion).
	s2 := NewStore()
	s2.Update("easee", DerEV, 3600, nil, nil)
	s2.DriverHealthMut("easee").RecordSuccess()
	if got := s2.SumOnlineEVW(); got != 3600 {
		t.Errorf("real EV reading must pass through, got %g", got)
	}
}
