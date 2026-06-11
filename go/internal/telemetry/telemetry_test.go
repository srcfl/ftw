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
	s.Update("a", DerPV, -300, nil, nil)

	if r := s.Get("a", DerBattery); r == nil || r.RawW != 100 {
		t.Error("a:battery")
	}
	if r := s.Get("b", DerBattery); r == nil || r.RawW != 200 {
		t.Error("b:battery")
	}
	if r := s.Get("a", DerPV); r == nil || r.RawW != -300 {
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
	h.RecordError("e1")
	h.RecordError("e2")
	h.RecordError("e3")
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

func TestStoreRejectsInvalidReadings(t *testing.T) {
	s := NewStore()
	s.Update("pv", DerPV, 500, nil, nil)
	if r := s.Get("pv", DerPV); r != nil {
		t.Fatalf("positive pv reading must be rejected, got %+v", r)
	}

	s.Update("pv", DerPV, -1200, nil, nil)
	if r := s.Get("pv", DerPV); r == nil || r.RawW != -1200 {
		t.Fatalf("valid pv reading should be stored, got %+v", r)
	}

	s.Update("pv", DerPV, math.NaN(), nil, nil)
	if r := s.Get("pv", DerPV); r == nil || r.RawW != -1200 {
		t.Fatalf("non-finite update should not overwrite last valid reading, got %+v", r)
	}

	badSoC := 42.0
	s.Update("bat", DerBattery, 0, &badSoC, nil)
	if r := s.Get("bat", DerBattery); r != nil {
		t.Fatalf("battery percent soc must be rejected at telemetry boundary, got %+v", r)
	}
}

func TestEmitMetricRejectsNonFinite(t *testing.T) {
	s := NewStore()
	s.EmitMetric("driver", "bad", math.Inf(1))
	if samples := s.FlushSamples(); len(samples) != 0 {
		t.Fatalf("non-finite metric should be dropped, got %+v", samples)
	}

	s.EmitMetric("driver", "ok", 42)
	samples := s.FlushSamples()
	if len(samples) != 1 || samples[0].Metric != "ok" || samples[0].Value != 42 {
		t.Fatalf("valid metric not buffered as expected: %+v", samples)
	}
}

// ---- DerType ----

func TestDerTypeRoundtrip(t *testing.T) {
	for _, name := range []string{"meter", "pv", "battery", "ev", "v2x_charger", "vehicle"} {
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

func TestV2XAcceptsSignedPowerAndVehicleSoC(t *testing.T) {
	s := NewStore()
	soc := 0.58
	s.Update("dc2", DerV2X, -4200, &soc, nil)
	r := s.Get("dc2", DerV2X)
	if r == nil || r.RawW != -4200 || r.SoC == nil || *r.SoC != 0.58 {
		t.Fatalf("v2x signed reading not stored as expected: %+v", r)
	}
	badSoC := 58.0
	s.Update("bad-dc2", DerV2X, 1000, &badSoC, nil)
	if r := s.Get("bad-dc2", DerV2X); r != nil {
		t.Fatalf("v2x percent SoC should be rejected, got %+v", r)
	}
}

func TestSumOnlineV2XWIsSignedAndSkipsOffline(t *testing.T) {
	s := NewStore()
	s.Update("charging", DerV2X, 3000, nil, nil)
	s.Update("discharging", DerV2X, -1200, nil, nil)
	s.Update("offline", DerV2X, 9000, nil, nil)
	s.DriverHealthMut("charging").RecordSuccess()
	s.DriverHealthMut("discharging").RecordSuccess()
	s.DriverHealthMut("offline").SetOffline()
	if got := s.SumOnlineV2XW(); got != 1800 {
		t.Fatalf("signed online V2X sum: got %f, want 1800", got)
	}
}

func TestWatchdogPerDriverOverride(t *testing.T) {
	s := NewStore()
	// Two drivers: one with the site-default tolerance, one with a
	// 5-minute override mimicking the Tesla driver.
	tesla := s.DriverHealthMut("tesla")
	tesla.RecordSuccess()
	pixii := s.DriverHealthMut("pixii")
	pixii.RecordSuccess()

	s.SetDriverWatchdogTimeout("tesla", 5*time.Minute)

	// Rewind both LastSuccess to 90 s ago (>60 s site default but
	// <5 min tesla override).
	t90 := time.Now().Add(-90 * time.Second)
	s.health["tesla"].LastSuccess = &t90
	s.health["pixii"].LastSuccess = &t90

	transitions := s.WatchdogScan(60 * time.Second)

	// Pixii should flip offline (uses site default), tesla stays online
	// (override is 5 min).
	flipped := map[string]bool{}
	for _, tr := range transitions {
		flipped[tr.Name] = !tr.Online
	}
	if !flipped["pixii"] {
		t.Errorf("pixii should be flagged stale under 60s site default, got transitions=%+v", transitions)
	}
	if flipped["tesla"] {
		t.Errorf("tesla should remain online under 5-min override at 90s stale; transitions=%+v", transitions)
	}

	// Now push tesla past its 5-min override.
	t6 := time.Now().Add(-6 * time.Minute)
	s.health["tesla"].LastSuccess = &t6
	transitions = s.WatchdogScan(60 * time.Second)
	flipped = map[string]bool{}
	for _, tr := range transitions {
		flipped[tr.Name] = !tr.Online
	}
	if !flipped["tesla"] {
		t.Errorf("tesla should flip stale at 6 min under 5-min override; transitions=%+v", transitions)
	}
}
