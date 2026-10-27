package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The site meter's reads fail (held reading, no new UpdatedAt) while the
// battery keeps reporting fresh telemetry. Within the 60 s watchdog window
// dispatch must not act on the held meter value again and again.
func runFrozenMeter(t *testing.T, heldGridW, realHouseW float64) (lastCmd, realGrid float64) {
	t.Helper()
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, heldGridW, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	soc := 0.5
	store.Update("bat", telemetry.DerBattery, 0, &soc, nil)
	store.DriverHealthMut("bat").RecordSuccess()

	st := NewState(0, 50, "meter")
	st.Mode = ModeSelfConsumption
	clk := time.Now()
	st.clock = func() time.Time { return clk }
	capsMap := caps(map[string]float64{"bat": 10000})
	st.DriverLimits = map[string]PowerLimits{"bat": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	frozenAt := store.Get("meter", telemetry.DerMeter).UpdatedAt

	batW := 0.0
	for tick := 0; tick < 10; tick++ { // 10 ticks x 5 s = 50 s < 60 s watchdog
		// Meter poll fails: RecordError, reading held.
		store.DriverHealthMut("meter").RecordError("modbus timeout")
		targets := ComputeDispatch(store, st, capsMap, 3*25*230)
		for _, tg := range targets {
			if tg.Driver == "bat" {
				batW = tg.TargetW
			}
		}
		// Battery follows its command and reports it (fresh).
		for i := 0; i < 30; i++ {
			store.Update("bat", telemetry.DerBattery, batW, &soc, nil)
		}
		store.DriverHealthMut("bat").RecordSuccess()
		t.Logf("tick %d: bat cmd %.0f W, real grid %.0f W, meter says %.0f W",
			tick, batW, realHouseW+batW, store.Get("meter", telemetry.DerMeter).SmoothedW)
		clk = clk.Add(5 * time.Second)
	}
	if !store.Get("meter", telemetry.DerMeter).UpdatedAt.Equal(frozenAt) {
		t.Fatal("meter reading moved")
	}
	return batW, realHouseW + batW
}

func TestFrozenImportMeterDoesNotDischargeToExport(t *testing.T) {
	cmd, grid := runFrozenMeter(t, 2000, 2000)
	if grid < -500 {
		t.Fatalf("frozen +2000 W meter drove battery to %.0f W; real grid %.0f W (export)", cmd, grid)
	}
}

func TestFrozenExportMeterDoesNotChargeFromGrid(t *testing.T) {
	cmd, grid := runFrozenMeter(t, -3000, 1500)
	if grid > 3000 {
		t.Fatalf("frozen -3000 W meter drove battery to %.0f W; real grid %.0f W (import)", cmd, grid)
	}
}

// A meter that stops updating after a good sample holds the battery at its
// last order and lets a manual hold only cut toward 0. The next new sample
// releases the hold.
func TestHeldMeterOnlyCutsTowardZero(t *testing.T) {
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{{"bat", 0, 0.5}})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.MinDispatchIntervalS = 0
	st.SlewEnabled = false
	capsMap := caps(map[string]float64{"bat": 10000})
	st.DriverLimits = map[string]PowerLimits{"bat": {MaxChargeW: 10000, MaxDischargeW: 10000}}

	first := targetFor(t, ComputeDispatch(store, st, capsMap, 3*25*230), "bat").TargetW
	if first >= 0 {
		t.Fatalf("fresh +2000 W import did not discharge: %.0f W", first)
	}
	store.Update("bat", telemetry.DerBattery, first, ptrF64(0.5), nil)
	if got := ComputeDispatch(store, st, capsMap, 3*25*230); len(got) != 0 {
		t.Fatalf("held meter sample produced new orders: %+v", got)
	}

	st.SetBatteryManualHold(BatteryManualHold{PowerW: first / 2, ExpiresAt: time.Now().Add(time.Hour)})
	if got := targetFor(t, ComputeDispatch(store, st, capsMap, 3*25*230), "bat").TargetW; got != first/2 {
		t.Fatalf("cut toward 0 on a held sample: got %.0f W, want %.0f W", got, first/2)
	}
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 3000, ExpiresAt: time.Now().Add(time.Hour)})
	if got := targetFor(t, ComputeDispatch(store, st, capsMap, 3*25*230), "bat").TargetW; got != 0 {
		t.Fatalf("held sample allowed a reversal to charge: %.0f W", got)
	}

	store.Update("ferroamp", telemetry.DerMeter, 2000, nil, nil)
	if got := targetFor(t, ComputeDispatch(store, st, capsMap, 3*25*230), "bat").TargetW; got != 3000 {
		t.Fatalf("new sample did not release the hold: %.0f W", got)
	}
}

func heldMeterState() (*State, map[string]float64) {
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.MinDispatchIntervalS = 0
	st.SlewEnabled = false
	st.DriverLimits = map[string]PowerLimits{"bat": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	return st, caps(map[string]float64{"bat": 10000})
}

func reportBattery(store *telemetry.Store, w float64, data json.RawMessage) {
	soc := 0.5
	for i := 0; i < 40; i++ {
		store.Update("bat", telemetry.DerBattery, w, &soc, data)
	}
	store.DriverHealthMut("bat").RecordSuccess()
}

func sentTo(targets []DispatchTarget, driver string) *DispatchTarget {
	for i := range targets {
		if targets[i].Driver == driver {
			return &targets[i]
		}
	}
	return nil
}

// A held meter must not keep a battery discharging after max_discharge_w
// becomes an explicit 0. The deadband exit falls through for this; so must
// the held exit.
func TestHeldMeterStillSendsExplicitZeroDischarge(t *testing.T) {
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{{"bat", 0, 0.5}})
	st, capsMap := heldMeterState()
	first := targetFor(t, ComputeDispatch(store, st, capsMap, 3*25*230), "bat").TargetW
	if first >= 0 {
		t.Fatalf("fresh +2000 W import did not discharge: %.0f W", first)
	}
	reportBattery(store, first, nil)
	st.DriverLimits = map[string]PowerLimits{"bat": {MaxChargeW: 10000, MaxDischargeW: 0, MaxDischargeWSet: true}}
	sent := sentTo(ComputeDispatch(store, st, capsMap, 3*25*230), "bat")
	if sent == nil || sent.TargetW < 0 {
		t.Fatalf("held meter left battery discharging at %.0f W under max_discharge_w=0: %+v", first, sent)
	}
}

// A held meter must not keep a battery charging after its driver reports
// charge_capable=false.
func TestHeldMeterStillStopsBlockedCharge(t *testing.T) {
	store := seedStore(-2000, []struct {
		name          string
		currentW, soc float64
	}{{"bat", 0, 0.5}})
	st, capsMap := heldMeterState()
	first := targetFor(t, ComputeDispatch(store, st, capsMap, 3*25*230), "bat").TargetW
	if first <= 0 {
		t.Fatalf("fresh -2000 W export did not charge: %.0f W", first)
	}
	reportBattery(store, first, json.RawMessage(`{"charge_capable":false}`))
	sent := sentTo(ComputeDispatch(store, st, capsMap, 3*25*230), "bat")
	if sent == nil || sent.TargetW > 0 {
		t.Fatalf("held meter left battery charging at %.0f W after charge_capable=false: %+v", first, sent)
	}
}

// A meter poll that emits and then returns an error leaves the latest read
// marked failed on every tick, but each tick still brings a new sample.
// Dispatch must act on it from a fresh start.
func TestEmitThenErrorMeterIsNotHeld(t *testing.T) {
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{{"bat", 0, 0.5}})
	st, capsMap := heldMeterState()
	acted := false
	for tick := 0; tick < 10; tick++ {
		time.Sleep(time.Millisecond) // UpdatedAt is wall-clock time
		store.Update("ferroamp", telemetry.DerMeter, 2000, nil, nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
		store.DriverHealthMut("ferroamp").RecordError("secondary register timeout")
		if sent := sentTo(ComputeDispatch(store, st, capsMap, 3*25*230), "bat"); sent != nil && sent.TargetW < 0 {
			acted = true
		}
	}
	if !acted {
		t.Fatal("ten new +2000 W meter samples, battery never dispatched")
	}
}
