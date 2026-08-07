package control

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// emitV2X pushes a bidirectional-charger reading with an explicit SoC
// (V2X readings without one are rejected at validation).
func emitV2X(t *testing.T, s *telemetry.Store, driver string, w, soc float64) {
	t.Helper()
	s.DriverHealthMut(driver).RecordSuccess()
	s.Update(driver, telemetry.DerV2X, w, &soc, nil)
}

func solarFeedState(drivers ...string) *State {
	st := NewState(0, 100, "meter")
	st.SolarFeedDrivers = map[string]bool{}
	for _, d := range drivers {
		st.SolarFeedDrivers[d] = true
	}
	return st
}

func TestComputeSolarFeed_NoArmedDrivers_ReturnsNil(t *testing.T) {
	st := NewState(0, 100, "meter")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -3000)
	emitPV(t, store, "pv", -5000)
	if got := ComputeSolarFeed(st, store); got != nil {
		t.Errorf("expected nil with no armed drivers; got %+v", got)
	}
}

func TestComputeSolarFeed_ExportSmallerThanPV_SendsExport(t *testing.T) {
	st := solarFeedState("nibe")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -3000)
	emitPV(t, store, "pv", -5000)
	got := ComputeSolarFeed(st, store)
	if len(got) != 1 || got[0].Driver != "nibe" || got[0].PowerW != -3000 {
		t.Errorf("want [{nibe -3000}]; got %+v", got)
	}
}

func TestComputeSolarFeed_ExportLargerThanPV_CapsAtPVGeneration(t *testing.T) {
	st := solarFeedState("nibe")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -5000)
	emitPV(t, store, "pv", -3000)
	got := ComputeSolarFeed(st, store)
	if len(got) != 1 || got[0].PowerW != -3000 {
		t.Errorf("want PowerW -3000 (PV-capped); got %+v", got)
	}
}

func TestComputeSolarFeed_Importing_SendsZero(t *testing.T) {
	st := solarFeedState("nibe")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", 500)
	emitPV(t, store, "pv", -5000)
	got := ComputeSolarFeed(st, store)
	if len(got) != 1 || got[0].PowerW != 0 {
		t.Errorf("want PowerW 0 while importing; got %+v", got)
	}
}

func TestComputeSolarFeed_BatteryDischargeIsNotSolar(t *testing.T) {
	st := solarFeedState("nibe")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -4000)
	emitPV(t, store, "pv", -5000)
	emitBattery(t, store, "bat", -3000, 0.5)
	got := ComputeSolarFeed(st, store)
	if len(got) != 1 || got[0].PowerW != -1000 {
		t.Errorf("want PowerW -1000 (export minus battery discharge); got %+v", got)
	}
}

func TestComputeSolarFeed_ChargingBatteryDoesNotInflateSurplus(t *testing.T) {
	st := solarFeedState("nibe")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -1000)
	emitPV(t, store, "pv", -5000)
	emitBattery(t, store, "bat", 2000, 0.5)
	got := ComputeSolarFeed(st, store)
	if len(got) != 1 || got[0].PowerW != -1000 {
		t.Errorf("want PowerW -1000 (charge draw must not add surplus); got %+v", got)
	}
}

func TestComputeSolarFeed_V2XDischargeIsNotSolar(t *testing.T) {
	st := solarFeedState("nibe")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -2000)
	emitPV(t, store, "pv", -5000)
	emitV2X(t, store, "car", -1500, 0.7)
	got := ComputeSolarFeed(st, store)
	if len(got) != 1 || got[0].PowerW != -500 {
		t.Errorf("want PowerW -500 (export minus V2X discharge); got %+v", got)
	}
}

func TestComputeSolarFeed_NoPVTelemetry_SendsZero(t *testing.T) {
	st := solarFeedState("nibe")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -2000)
	got := ComputeSolarFeed(st, store)
	if len(got) != 1 || got[0].PowerW != 0 {
		t.Errorf("want PowerW 0 without PV telemetry; got %+v", got)
	}
}

func TestComputeSolarFeed_OfflinePVDriver_SendsZero(t *testing.T) {
	st := solarFeedState("nibe")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -2000)
	// PV reading present but its driver never recorded a success — the
	// generation of an offline driver must not count as live surplus.
	store.Update("pv", telemetry.DerPV, -5000, nil, nil)
	got := ComputeSolarFeed(st, store)
	if len(got) != 1 || got[0].PowerW != 0 {
		t.Errorf("want PowerW 0 with only offline PV; got %+v", got)
	}
}

func TestComputeSolarFeed_MissingMeter_SendsZero(t *testing.T) {
	st := solarFeedState("nibe")
	store := telemetry.NewStore()
	emitPV(t, store, "pv", -5000)
	got := ComputeSolarFeed(st, store)
	if len(got) != 1 || got[0].PowerW != 0 {
		t.Errorf("want PowerW 0 without a site-meter reading; got %+v", got)
	}
}

func TestComputeSolarFeed_MultipleDrivers_SameValueSorted(t *testing.T) {
	st := solarFeedState("zeta", "alpha")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -3000)
	emitPV(t, store, "pv", -5000)
	got := ComputeSolarFeed(st, store)
	if len(got) != 2 || got[0].Driver != "alpha" || got[1].Driver != "zeta" {
		t.Fatalf("want sorted [alpha zeta]; got %+v", got)
	}
	if got[0].PowerW != -3000 || got[1].PowerW != -3000 {
		t.Errorf("want the same site-wide value for both; got %+v", got)
	}
}
