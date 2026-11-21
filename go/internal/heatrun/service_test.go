package heatrun

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/heatplan"
	"github.com/srcfl/ftw/go/internal/thermal"
	"github.com/srcfl/ftw/go/internal/thermaltwin"
)

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }

type fakeWriter struct {
	holds map[string]struct {
		control string
		exp     int64
	}
	sent     []float64
	released []string
	now      func() time.Time
}

func newWriter(now func() time.Time) *fakeWriter {
	return &fakeWriter{holds: map[string]struct {
		control string
		exp     int64
	}{}, now: now}
}

func (w *fakeWriter) Hold(_ context.Context, driver, control string, v float64, d time.Duration) (int64, error) {
	exp := w.now().Add(d).UnixMilli()
	w.holds[driver] = struct {
		control string
		exp     int64
	}{control, exp}
	w.sent = append(w.sent, v)
	return exp, nil
}

func (w *fakeWriter) Active(driver string) (string, int64, bool) {
	h, ok := w.holds[driver]
	if !ok || h.exp <= w.now().UnixMilli() {
		return "", 0, false
	}
	return h.control, h.exp, true
}

func (w *fakeWriter) Release(_ context.Context, driver string) error {
	delete(w.holds, driver)
	w.released = append(w.released, driver)
	return nil
}

type house struct {
	now    time.Time
	cfg    *config.Heat
	status thermaltwin.Status
	price  func(h float64) float64
	floor  float64
	w      *fakeWriter

	meterStale bool
}

// A bathroom floor the size of the one that built this: 0.6 kW, coasting
// some ten hours, regulated on its floor sensor by an air thermostat.
func newHouse() *house {
	h := &house{
		now:    time.Date(2026, 11, 17, 12, 0, 0, 0, time.Local),
		status: thermaltwin.StatusTrusted,
		floor:  23,
		price: func(hr float64) float64 {
			if hr >= 17 && hr < 20 {
				return 450
			}
			return 100
		},
	}
	h.cfg = &config.Heat{Write: true, Zones: []config.HeatZone{{
		Name: "badrum", Sensor: "wiser_7/floor_temp_c", Control: "wiser_7/set_setpoint",
		Boundary: "wiser_7/room_temp_c", Source: "direct_electric", HeatKW: 0.6,
		TargetC: 24.5, MinC: 20, MaxC: 27, ComfortWeight: 2,
	}}}
	h.w = newWriter(func() time.Time { return h.now })
	return h
}

func (h *house) service() *Service {
	return New(Inputs{
		Config: func() *config.Heat { return h.cfg },
		Twins: func() []thermaltwin.ZoneStatus {
			return []thermaltwin.ZoneStatus{{
				Zone:   thermaltwin.Zone{Key: "wiser_7/floor_temp_c"},
				Status: h.status,
				Params: thermaltwin.Params{TauH: fp(10), CapacityKWhPerK: fp(0.4)},
			}}
		},
		Latest: func(driver, metric string) (float64, time.Time, bool) {
			switch metric {
			case "floor_temp_c":
				return h.floor, h.now, true
			case "room_temp_c":
				return 21, h.now, true
			}
			return 0, time.Time{}, false
		},
		Prices: func(from, to time.Time) []Price {
			var out []Price
			for t := from.Truncate(time.Hour); t.Before(to); t = t.Add(time.Hour) {
				out = append(out, Price{Start: t, Len: time.Hour, Value: h.price(float64(t.Hour()))})
			}
			// Prices known until midnight tomorrow.
			end := time.Date(2026, 11, 19, 0, 0, 0, 0, time.Local)
			var cut []Price
			for _, p := range out {
				if p.Start.Before(end) {
					cut = append(cut, p)
				}
			}
			return cut
		},
		Control: func(driver, control string) (drivers.CatalogControl, bool) {
			if driver != "wiser_7" || control != "set_setpoint" {
				return drivers.CatalogControl{}, false
			}
			return drivers.CatalogControl{ID: control,
				Input:   drivers.CatalogControlInput{Type: "number", Min: fp(5), Max: fp(30), Step: fp(0.5)},
				Thermal: &drivers.CatalogThermalControl{Asset: thermal.AssetSpaceHeat, Strategy: thermal.InfluenceSetpoint, MinIntervalS: ip(300), DeviceHoldMaxS: ip(1800)},
			}, true
		},
		Writer:    h.w,
		SiteFresh: func() bool { return !h.meterStale },
		Now:       func() time.Time { return h.now },
	})
}

func TestShadowPlansAndWritesNothing(t *testing.T) {
	h := newHouse()
	h.cfg.Write = false
	s := h.service()
	s.Tick(context.Background())
	snap := s.Snapshot()
	if !snap.Enabled || snap.Write {
		t.Fatalf("enabled %v write %v", snap.Enabled, snap.Write)
	}
	z := snap.Zones[0]
	if !z.Planned || z.Value == nil || len(z.Slots) == 0 {
		t.Fatalf("zone not planned: %+v", z)
	}
	if !strings.HasPrefix(z.Write, "shadow") || len(h.w.sent) != 0 {
		t.Fatalf("write %q, sent %v", z.Write, h.w.sent)
	}
}

// Before a peak the plan stores heat in the floor: it asks for more than the
// device's own setpoint, and with writing on and the twin trusted it is held.
func TestTrustedZoneIsWrittenAndHeldOnce(t *testing.T) {
	h := newHouse()
	h.now = time.Date(2026, 11, 17, 15, 0, 0, 0, time.Local)
	h.floor = 22
	s := h.service()
	s.Tick(context.Background())
	z := s.Snapshot().Zones[0]
	if len(h.w.sent) != 1 || !strings.HasPrefix(z.Write, "wrote") {
		t.Fatalf("write %q, sent %v, value %v neutral %v", z.Write, h.w.sent, deref(z.Value), deref(z.Neutral))
	}
	// The next quarter with the same value does not write again while the
	// hold has time left.
	h.now = h.now.Add(slot)
	s.Tick(context.Background())
	if z := s.Snapshot().Zones[0]; len(h.w.sent) != 1 && !strings.HasPrefix(z.Write, "wrote") {
		t.Fatalf("second tick: %q, sent %v", z.Write, h.w.sent)
	}
	s.Stop()
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestUntrustedTwinOrClosedGateWritesNothingAndHandsBack(t *testing.T) {
	h := newHouse()
	h.now = time.Date(2026, 11, 17, 15, 0, 0, 0, time.Local)
	h.floor = 22
	s := h.service()
	s.Tick(context.Background())
	if len(h.w.sent) != 1 {
		t.Fatalf("setup: sent %v (%q)", h.w.sent, s.Snapshot().Zones[0].Write)
	}

	h.status = thermaltwin.StatusLearning
	h.now = h.now.Add(slot)
	s.Tick(context.Background())
	z := s.Snapshot().Zones[0]
	if !strings.HasPrefix(z.Write, "released") || len(h.w.released) != 1 {
		t.Fatalf("untrusted: %q, released %v", z.Write, h.w.released)
	}

	h.status = thermaltwin.StatusTrusted
	h.cfg.Savings = &heatplan.SavingsGate{MinPerDay: 1e6}
	h.now = h.now.Add(slot)
	s.Tick(context.Background())
	snap := s.Snapshot()
	if snap.Gate.Open || !strings.Contains(snap.Zones[0].Write, "least saving") || len(h.w.sent) != 1 {
		t.Fatalf("closed gate: %+v, %q, sent %v", snap.Gate, snap.Zones[0].Write, h.w.sent)
	}
}

// An operator's hold on the driver is not overwritten and not released.
func TestAnOperatorsHoldIsLeftAlone(t *testing.T) {
	h := newHouse()
	h.now = time.Date(2026, 11, 17, 15, 0, 0, 0, time.Local)
	h.floor = 22
	h.w.holds["wiser_7"] = struct {
		control string
		exp     int64
	}{"set_setpoint", h.now.Add(time.Hour).UnixMilli()}
	s := h.service()
	s.Tick(context.Background())
	z := s.Snapshot().Zones[0]
	if !strings.Contains(z.Write, "someone else") || len(h.w.sent) != 0 {
		t.Fatalf("%q, sent %v", z.Write, h.w.sent)
	}
	h.cfg.Write = false
	s.Tick(context.Background())
	if len(h.w.released) != 0 {
		t.Fatalf("released an operator's hold: %v", h.w.released)
	}
}

// Writing off after writing hands the device back at once.
func TestTurningWritingOffHandsTheDeviceBack(t *testing.T) {
	h := newHouse()
	h.now = time.Date(2026, 11, 17, 15, 0, 0, 0, time.Local)
	h.floor = 22
	s := h.service()
	s.Tick(context.Background())
	h.cfg.Write = false
	h.now = h.now.Add(slot)
	s.Tick(context.Background())
	if len(h.w.released) != 1 {
		t.Fatalf("released %v after writing was turned off", h.w.released)
	}
}

func TestZoneThatCannotBePlannedSaysWhy(t *testing.T) {
	for name, tc := range map[string]struct {
		mod  func(h *house)
		want string
	}{
		"stale sensor":       {func(h *house) { h.floor = 22; h.now = h.now.Add(0) }, ""},
		"no model":           {func(h *house) { h.cfg.Zones[0].Sensor = "wiser_8/floor_temp_c" }, "no thermal twin"},
		"undeclared control": {func(h *house) { h.cfg.Zones[0].Control = "wiser_7/other" }, "not a declared control"},
	} {
		if tc.want == "" {
			continue
		}
		h := newHouse()
		tc.mod(h)
		s := h.service()
		s.Tick(context.Background())
		z := s.Snapshot().Zones[0]
		if z.Planned || !strings.Contains(z.Reason, tc.want) {
			t.Errorf("%s: planned %v reason %q", name, z.Planned, z.Reason)
		}
	}
}

func TestCandidatesStayWithinSixteenAndKeepTheEnds(t *testing.T) {
	got := candidates(20, 27, 0.5)
	if len(got) > 16 || got[0] != 20 || got[len(got)-1] > 27 {
		t.Fatalf("candidates %v", got)
	}
	if got := candidates(20, 22, 0.5); len(got) != 5 {
		t.Fatalf("candidates %v", got)
	}
	if candidates(22, 20, 0.5) != nil {
		t.Fatal("an empty range gave candidates")
	}
}

// Stale site-meter data stops dispatch: the device goes back to itself.
func TestStaleSiteMeterStopsWriting(t *testing.T) {
	h := newHouse()
	h.now = time.Date(2026, 11, 17, 15, 0, 0, 0, time.Local)
	h.floor = 22
	s := h.service()
	s.Tick(context.Background())
	if len(h.w.sent) != 1 {
		t.Fatalf("setup: sent %v", h.w.sent)
	}
	h.meterStale = true
	h.now = h.now.Add(slot)
	s.Tick(context.Background())
	if z := s.Snapshot().Zones[0]; !strings.Contains(z.Write, "site meter") || len(h.w.released) != 1 {
		t.Fatalf("%q, released %v", z.Write, h.w.released)
	}
}

// A device that drops a written value after five minutes is written again
// between plans, and stops being written when the meter goes stale.
func TestShortDeviceHoldIsRenewedBetweenPlans(t *testing.T) {
	h := newHouse()
	h.now = time.Date(2026, 11, 17, 15, 0, 0, 0, time.Local)
	h.floor = 22
	s := h.service()
	s.in.Control = func(driver, control string) (drivers.CatalogControl, bool) {
		return drivers.CatalogControl{ID: control,
			Input:   drivers.CatalogControlInput{Type: "number", Min: fp(5), Max: fp(30), Step: fp(0.5)},
			Thermal: &drivers.CatalogThermalControl{Asset: thermal.AssetSpaceHeat, Strategy: thermal.InfluenceSetpoint, DeviceHoldMaxS: ip(300)},
		}, true
	}
	s.Tick(context.Background())
	if len(h.w.sent) != 1 {
		t.Fatalf("setup: sent %v (%q)", h.w.sent, s.Snapshot().Zones[0].Write)
	}
	for i := 0; i < 28; i++ { // 14 minutes in 30 s steps
		h.now = h.now.Add(refreshEvery)
		s.Refresh(context.Background())
		if _, _, ok := h.w.Active("wiser_7"); !ok {
			t.Fatalf("the hold lapsed after %v", time.Duration(i+1)*refreshEvery)
		}
	}
	if len(h.w.sent) < 3 || len(h.w.sent) > 6 {
		t.Fatalf("renewed %d times in 14 minutes of a 5-minute hold", len(h.w.sent)-1)
	}
	h.meterStale = true
	s.Refresh(context.Background())
	if len(h.w.released) != 1 {
		t.Fatalf("a stale meter did not hand the device back: %v", h.w.released)
	}
}

// The house heated by a CTC on a heat curve, its rooms read by an
// underfloor gateway: a heat pump zone planned on the outdoor forecast, in
// shadow, and through a curve offset once the pump declares one.
func heatPumpHouse(h *house, outdoor bool) *Service {
	h.cfg = &config.Heat{Zones: []config.HeatZone{{
		Name: "house", Sensor: "wiser_7/floor_temp_c", HeatInput: "CTC/space_heat_w",
		Boundary: "outdoor", Source: "heat_pump", HeatKW: 6,
		TargetC: 21, MinC: 20, MaxC: 22.5, ComfortWeight: 2,
		HeatPump: &config.HeatPump{Supply: &config.HeatSupplyCurve{ColdOutdoorC: -20, ColdSupplyC: 45, MildOutdoorC: 15, MildSupplyC: 24}},
	}}}
	h.floor = 21
	s := h.service()
	s.in.Twins = func() []thermaltwin.ZoneStatus {
		return []thermaltwin.ZoneStatus{{Zone: thermaltwin.Zone{Key: "wiser_7/floor_temp_c"}, Status: h.status,
			Params: thermaltwin.Params{TauH: fp(60), CapacityKWhPerK: fp(5)}}}
	}
	if outdoor {
		s.in.Outdoor = func(_ time.Time, n int) ([]float64, bool) { return constant(n, 0), true }
	}
	return s
}

func TestHeatPumpZoneNeedsTheForecastAndPlansWithIt(t *testing.T) {
	h := newHouse()
	s := heatPumpHouse(h, false)
	s.Tick(context.Background())
	if z := s.Snapshot().Zones[0]; z.Planned || !strings.Contains(z.Reason, "outdoor forecast") {
		t.Fatalf("without a forecast: planned %v, %q", z.Planned, z.Reason)
	}
	h = newHouse()
	s = heatPumpHouse(h, true)
	s.Tick(context.Background())
	if z := s.Snapshot().Zones[0]; !z.Planned || z.Value == nil {
		t.Fatalf("with a forecast: %+v", z)
	}
}

func TestCurveOffsetIsPlannedOnlyWithItsEffect(t *testing.T) {
	h := newHouse()
	s := heatPumpHouse(h, true)
	h.cfg.Zones[0].Control = "CTC/set_heat_curve_offset"
	s.in.Control = func(driver, control string) (drivers.CatalogControl, bool) {
		return drivers.CatalogControl{ID: control,
			Input:   drivers.CatalogControlInput{Type: "number", Min: fp(-5), Max: fp(5), Step: fp(1), Default: fp(0)},
			Thermal: &drivers.CatalogThermalControl{Asset: thermal.AssetSpaceHeat, Strategy: thermal.InfluenceCurveOffset},
		}, true
	}
	s.Tick(context.Background())
	if z := s.Snapshot().Zones[0]; z.Planned || !strings.Contains(z.Reason, "curve_effect") {
		t.Fatalf("without an effect: planned %v, %q", z.Planned, z.Reason)
	}
	h.cfg.Zones[0].CurveEffect = &config.CurveEffect{ZoneKPerUnit: 0.5, SupplyKPerUnit: 1}
	s.Tick(context.Background())
	z := s.Snapshot().Zones[0]
	if !z.Planned || z.Neutral == nil || *z.Neutral != 0 {
		t.Fatalf("with an effect: %+v", z)
	}
	for _, sl := range z.Slots {
		if sl.Value < -5 || sl.Value > 5 {
			t.Fatalf("offset %v outside the declared range", sl.Value)
		}
	}
}
