package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// phaseMeter is one site reading with signed phase currents and powers.
func phaseMeter(t *testing.T, at time.Time, phaseA [3]float64) *telemetry.DerReading {
	t.Helper()
	data, err := json.Marshal(map[string]float64{
		"l1_a": phaseA[0], "l2_a": phaseA[1], "l3_a": phaseA[2],
		"l1_w": phaseA[0] * 230, "l2_w": phaseA[1] * 230, "l3_w": phaseA[2] * 230,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &telemetry.DerReading{RawW: (phaseA[0] + phaseA[1] + phaseA[2]) * 230, Data: data, UpdatedAt: at}
}

func phaseState(fuseA, marginA float64) *State {
	st := NewState(0, 0, "meter")
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases, st.SiteFuseSafetyA = fuseA, 230, 3, marginA
	return st
}

// One reading of the fuse for every decision: one margin, at least 1 A;
// current with direction; room for import, room for export; an overage
// only once its heat budget is spent.
func TestFusePhasesReadsDirectionRoomAndOverage(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	st := phaseState(25, 0.5)
	f, ok := st.fusePhasesAt(phaseMeter(t, t0, [3]float64{20, -15, 5}))
	if !ok || f.limitA != 24 {
		t.Fatalf("limit %.1f A, want 25 less the 1 A margin", f.limitA)
	}
	if got := f.roomA([3]float64{}); got != 4 {
		t.Fatalf("import room %.1f A, want 24 - 20 = 4 A", got)
	}
	if got := f.roomA([3]float64{1, 1, 1}); got != 3 {
		t.Fatalf("import room after 1 A pending %.1f A, want 3 A", got)
	}
	if got := f.exportRoomA(); got != 9 {
		t.Fatalf("export room %.1f A, want 24 - 15 = 9 A", got)
	}
	if f.importOverA([3]float64{}) != 0 || f.exportOverA() != 0 {
		t.Fatalf("no phase is over: import %.1f A, export %.1f A", f.importOverA([3]float64{}), f.exportOverA())
	}

	// 26 A import on L1: the first reading sets the heat, so it is over at
	// once; a cut already sent covers part of it.
	st = phaseState(25, 1.5)
	f, _ = st.fusePhasesAt(phaseMeter(t, t0, [3]float64{26, -10, 0}))
	if f.limitA != 23.5 || f.importOverA([3]float64{}) != 2.5 || f.importOverA([3]float64{1, 1, 1}) != 1.5 {
		t.Fatalf("limit %.1f A, over %.1f A, want 23.5 A and 2.5 A (1.5 A after a 1 A cut)", f.limitA, f.importOverA([3]float64{}))
	}

	// A short spike from 20 A spends no heat budget yet: no overage, and
	// no room either.
	st = phaseState(25, 0)
	st.fusePhasesAt(phaseMeter(t, t0, [3]float64{20, 0, 0}))
	f, _ = st.fusePhasesAt(phaseMeter(t, t0.Add(time.Second), [3]float64{27, 0, 0}))
	if f.importOverA([3]float64{}) != 0 || f.roomA([3]float64{}) != -3 {
		t.Fatalf("spike: over %.1f A, room %.1f A; want 0 A and -3 A", f.importOverA([3]float64{}), f.roomA([3]float64{}))
	}

	// Export beyond the limit, heat spent: an export overage, not import.
	st = phaseState(25, 0)
	f, _ = st.fusePhasesAt(phaseMeter(t, t0, [3]float64{-27, 0, 0}))
	if f.importOverA([3]float64{}) != 0 || f.exportOverA() != 3 {
		t.Fatalf("export: import over %.1f A, export over %.1f A; want 0 and 3 A", f.importOverA([3]float64{}), f.exportOverA())
	}
}

// Without phase currents the total stands in, shared over the phases.
// Phase currents that cannot be read leave no room for a rise.
func TestFusePhasesWithoutReadablePhases(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	st := phaseState(25, 0)
	f, ok := st.fusePhasesAt(&telemetry.DerReading{RawW: 13800, UpdatedAt: t0})
	if !ok || f.measured || f.roomA([3]float64{}) != 4 {
		t.Fatalf("total 13.8 kW: room %.1f A (measured %v), want 24 - 20 = 4 A from the total", f.roomA([3]float64{}), f.measured)
	}
	data, _ := json.Marshal(map[string]float64{"l1_a": 10, "l2_a": 10})
	f, ok = st.fusePhasesAt(&telemetry.DerReading{RawW: 4600, Data: data, UpdatedAt: t0.Add(time.Second)})
	if !ok || !f.blind || f.roomA([3]float64{}) > 0 || f.readingRoomA() > 0 {
		t.Fatalf("L3 missing: room %.1f A (blind %v), want none", f.roomA([3]float64{}), f.blind)
	}
	if _, ok := phaseState(0, 0).fusePhasesAt(&telemetry.DerReading{RawW: 1000}); ok {
		t.Fatal("no fuse configured, yet a fuse was read")
	}
}

// The site ceiling for batteries and EVs is the hour's import limit or the
// import at which the busiest phase meets the fuse, whichever is lower.
func TestSiteImportCeiling(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	st := phaseState(25, 0)
	meter := phaseMeter(t, t0, [3]float64{20, 5, 5})
	if got, byImport := st.siteImportCeilingW(meter, meter.RawW); got != meter.RawW+4*690 || byImport {
		t.Fatalf("ceiling %.0f W (by import %v), want the reading plus 4 A on three phases", got, byImport)
	}
	st.PeakImportCeilingW = 6000
	if got, byImport := st.siteImportCeilingW(meter, meter.RawW); got != 6000 || !byImport {
		t.Fatalf("ceiling %.0f W (by import %v), want the hour's 6,000 W", got, byImport)
	}
}

// A car that takes less than its offer earns no reservation: the meter
// shows the room, and the battery may charge into it (Fredrik, 21 October
// 2026). The car's offer is in force and in the reading; the battery keeps
// its planned charge while the busiest phase stays inside the limit.
func TestBatteryKeepsRoomACarDoesNotTake(t *testing.T) {
	store := telemetry.NewStore()
	data, _ := json.Marshal(map[string]float64{"l1_a": 22.9, "l2_a": 19.4, "l3_a": 19.4,
		"l1_w": 22.9 * 230, "l2_w": 19.4 * 230, "l3_w": 19.4 * 230})
	store.Update("meter", telemetry.DerMeter, 14200, nil, data)
	st := phaseState(25, 0.5)
	st.SetEVFuseDemands(nil)
	st.evLimit.offers = map[string]chargerOffers{"car": {driver: "easee", settle: evSettle,
		log: commandLog{{at: st.now().Add(-time.Hour), w: 11000, three: true}}}}
	st.batteryCommands = map[string]commandLog{"pixii": commandLog{}.add(st.now().Add(-time.Minute), 4750)}
	out := fitBatteryRisesToFuse([]DispatchTarget{{Driver: "pixii", TargetW: 4750}}, store, st)
	if out[0].TargetW != 4750 || out[0].Clamped {
		t.Fatalf("battery %+v, want its 4,750 W held", out[0])
	}
}

func TestPhaseImbalanceW(t *testing.T) {
	store := telemetry.NewStore()
	data, _ := json.Marshal(map[string]float64{"l1_a": 4 + 6.6, "l2_a": 0.5 + 6.6, "l3_a": 0.5 + 6.6})
	store.Update("meter", telemetry.DerMeter, 0, nil, data)
	st := phaseState(25, 0.5)
	want := (4 - 5.0/3) * 3 * 230
	if got := PhaseImbalanceW(store, st, time.Minute, time.Now()); math.Abs(got-want) > 1 {
		t.Fatalf("imbalance %.0f W, want %.0f W", got, want)
	}
	if got := PhaseImbalanceW(store, st, time.Minute, time.Now().Add(2*time.Minute)); got != 0 {
		t.Fatalf("stale meter gave imbalance %.0f W", got)
	}
}
