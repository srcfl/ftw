package control

import (
	"github.com/srcfl/ftw/go/internal/telemetry"
	"testing"
	"time"
)

func TestEVBatteryReliefRetentionRespectsEligibility(t *testing.T) {
	for _, mode := range []string{"healthy", "missing_soc", "empty", "blocked", "fault", "offline", "stale", "future", "no_order", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			store, state := evSplitFixture(t, 12000, []float64{21.5, 11.5, 11.5}, 10350, "3p")
			sent(state, "first", -1745, -1745)
			soc := .3
			store.Update("first", telemetry.DerBattery, -1745, &soc, nil)
			store.RecordDriverSuccess("first")
			r := store.Get("first", telemetry.DerBattery)
			switch mode {
			case "missing_soc":
				r.SoC = nil
			case "empty":
				x := .04
				r.SoC = &x
			case "blocked":
				r.Data = []byte(`{"discharge_capable":false}`)
			case "fault":
				store.DriverHealthMut("first").SetDeviceFault(true, "test")
			case "offline":
				store.DriverHealthMut("first").SetOffline()
			case "stale":
				r.UpdatedAt = r.UpdatedAt.Add(-time.Minute)
			case "future":
				r.UpdatedAt = state.now().Add(time.Minute)
			case "disabled":
				state.DriverLimits["first"] = PowerLimits{MaxDischargeW: 0, MaxDischargeWSet: true}
			case "no_order":
				delete(state.batteryCommands, "first")
			}
			out := retainEVBatteryRelief([]DispatchTarget{{Driver: "first", TargetW: 0}}, map[string]float64{"first": -1745}, 1725, store, state)
			want := 0.0
			if mode == "healthy" {
				want = -20
			}
			if got := targetW(out, "first"); got != want {
				t.Fatalf("got %.1f want %.1f", got, want)
			}
		})
	}
}
func TestEVBatteryPausedManagedLoadStillCapsCharge(t *testing.T) {
	store, state := evSplitFixture(t, 11000, []float64{21, 17, 17}, 0, "3p")
	state.evCommanded = map[string]float64{"car": 0}
	sent(state, "first", 2070, 2070)
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 5070}}, store, state, 17250)
	if got := targetW(out, "first"); got > 4140.01 {
		t.Fatalf("paused car released unmeasured room: %.1f", got)
	}
}

func TestEVBatteryPendingReliefRequiresAlignedFreshProof(t *testing.T) {
	for _, mode := range []string{"pending", "arrived", "stale", "future", "unavailable", "skew"} {
		t.Run(mode, func(t *testing.T) {
			store, state := evSplitFixture(t, 12000, []float64{21.5, 11.5, 11.5}, 10350, "3p")
			now := time.Now()
			state.clock = func() time.Time { return now }
			sent(state, "first", -1745, -690)
			soc := .3
			actual := -690.0
			if mode == "pending" {
				actual = -1745
			}
			store.Update("first", telemetry.DerBattery, actual, &soc, nil)
			store.RecordDriverSuccess("first")
			r := store.Get("first", telemetry.DerBattery)
			r.UpdatedAt = now
			meter := store.Get("meter", telemetry.DerMeter)
			meter.UpdatedAt = now
			switch mode {
			case "stale":
				r.UpdatedAt = now.Add(-time.Minute)
			case "future":
				r.UpdatedAt = now.Add(time.Second)
			case "unavailable":
				r.Data = []byte(`{"control_power_available":false}`)
			case "skew":
				r.UpdatedAt = now.Add(-11 * time.Second)
			}
			want := 1055.0
			if mode == "arrived" {
				want = 0
			}
			if got := pendingEVBatteryImportW(store, state, meter, map[string]float64{"first": -690}); got != want {
				t.Fatalf("pending %.1f want %.1f", got, want)
			}
		})
	}
}

// Releasing existing discharge is itself an import rise. Test the complete
// clamp, so a qualified helper alone cannot hide a missing pipeline call.
func TestEVBatteryClampRetainsReliefUntilImportFits(t *testing.T) {
	store, state := evSplitFixture(t, 12000, []float64{23.5, 13.5, 13.5}, 10350, "3p")
	sent(state, "ev", 10350, 10350)
	sent(state, "first", -1745, -1745)
	soc := .3
	store.Update("first", telemetry.DerBattery, -1745, &soc, nil)
	store.RecordDriverSuccess("first")
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 0}}, store, state, 17250)
	if got := targetW(out, "first"); got != -1400 {
		t.Fatalf("ending relief would exceed phase room: target %.1f want -1400", got)
	}
}

func TestEVBatteryClampDoesNotSpendUnseenImportTwice(t *testing.T) {
	for _, arrived := range []bool{false, true} {
		store, state := evSplitFixture(t, 12000, []float64{21.5, 11.5, 11.5}, 10350, "3p")
		sent(state, "ev", 10350, 10350)
		sent(state, "first", -1745, -690)
		soc, actual := .3, -1745.0
		if arrived {
			actual = -690
		}
		store.Update("first", telemetry.DerBattery, actual, &soc, nil)
		store.RecordDriverSuccess("first")
		out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 0}}, store, state, 17250)
		want := -20.0
		if arrived {
			want = 0
		}
		if got := targetW(out, "first"); got != want {
			t.Fatalf("arrived=%v target %.1f want %.1f", arrived, got, want)
		}
	}
}

func TestEVBatteryReliefDoesNotReserveUnadmittedDemand(t *testing.T) {
	store, state := evSplitFixture(t, 12000, []float64{21.5, 11.5, 11.5}, 10350, "3p")
	sent(state, "ev", 10350, 8280)
	sent(state, "first", -1745, -1745)
	store.Update("car", telemetry.DerEV, 8280, nil, nil)
	soc := .3
	store.Update("first", telemetry.DerBattery, -1745, &soc, nil)
	store.RecordDriverSuccess("first")
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 0}}, store, state, 17250)
	if got := targetW(out, "first"); got != -20 {
		t.Fatalf("unadmitted EV demand retained unnecessary discharge: %.1f", got)
	}
}
