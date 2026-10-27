package control

import (
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
