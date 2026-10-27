package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Slew anchors on measured SmoothedW. It must not walk a pack back into the
// discharge the distributor closed (SoC below the 5 % floor, or
// discharge_capable=false).
func TestSlewDoesNotReopenBlockedDischarge(t *testing.T) {
	cases := []struct {
		name       string
		soc        float64
		capability string
	}{
		{"soc_below_floor", 0.04, ""},
		{"discharge_capable_false", 0.50, `{"discharge_capable":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := NewState(0, 42, "meter")
			st.MinDispatchIntervalS = 0
			st.SlewRateW = 500
			store := seedDeadbandSite(2000, []deadbandBattery{{"bat", -3000, tc.soc, tc.capability}})
			out := ComputeDispatch(store, st, caps(map[string]float64{"bat": 10000}), 0)
			if got := targetsByDriver(out)["bat"].TargetW; got < -1 {
				t.Errorf("commanded %.0f W discharge to a pack that must not discharge", got)
			}
		})
	}
}

// When stopping the blocked pack at once would put the site over its fuse,
// the pack keeps only the discharge the fuse needs: 10000 + 3000 - 11040 W.
// That is less than the slewed ramp (-2500 W) and never more discharge.
func TestBlockedDischargeKeepsFuseOverage(t *testing.T) {
	st := NewState(0, 42, "meter")
	st.MinDispatchIntervalS = 0
	st.SlewRateW = 500
	store := seedDeadbandSite(10000, []deadbandBattery{{"bat", -3000, 0.04, ""}})
	out := ComputeDispatch(store, st, caps(map[string]float64{"bat": 10000}), 11040)
	if got := targetsByDriver(out)["bat"].TargetW; math.Abs(got+1960) > 1 {
		t.Errorf("target %.0f W, want -1960 W", got)
	}
}

// A stop 60 W over the fuse keeps 60 W of discharge, not the whole ramp.
func TestBlockedDischargeKeepsOnlyTheOverage(t *testing.T) {
	st := NewState(0, 42, "meter")
	st.MinDispatchIntervalS = 0
	st.SlewRateW = 500
	store := seedDeadbandSite(8100, []deadbandBattery{{"blk", -3000, 0.04, ""}})
	out := ComputeDispatch(store, st, caps(map[string]float64{"blk": 10000}), 11040)
	if got := targetsByDriver(out)["blk"].TargetW; math.Abs(got+60) > 1 {
		t.Errorf("target %.0f W, want -60 W", got)
	}
}

// A capable sibling takes the fuse relief, so the blocked pack stops.
func TestBlockedDischargeLeavesReliefToSibling(t *testing.T) {
	st := NewState(0, 42, "meter")
	st.MinDispatchIntervalS = 0
	st.SlewRateW = 500
	store := seedDeadbandSite(9000, []deadbandBattery{
		{"blk", -3000, 0.04, ""},
		{"ok", 0, 0.60, ""},
	})
	out := ComputeDispatch(store, st, caps(map[string]float64{"blk": 10000, "ok": 10000}), 11040)
	m := targetsByDriver(out)
	if m["blk"].TargetW < -1 {
		t.Errorf("blocked pack at %.0f W while the sibling had room", m["blk"].TargetW)
	}
	// 9000 W meter + 3000 W stop + the sibling's target must fit 11040 W.
	if grid := 12000 + m["ok"].TargetW; grid > 11041 {
		t.Errorf("projected grid %.0f W over the 11040 W fuse (sibling %.0f W)", grid, m["ok"].TargetW)
	}
}

// Two blocked packs share the discharge the fuse needs. Together they keep
// 6000 + 6000 - 11040 = 960 W, not two ramps of 2500 W.
func TestBlockedDischargeSharesOverage(t *testing.T) {
	st := NewState(0, 42, "meter")
	st.MinDispatchIntervalS = 0
	st.SlewRateW = 500
	store := seedDeadbandSite(6000, []deadbandBattery{
		{"a", -3000, 0.04, ""},
		{"b", -3000, 0.50, `{"discharge_capable":false}`},
	})
	out := ComputeDispatch(store, st, caps(map[string]float64{"a": 10000, "b": 10000}), 11040)
	m := targetsByDriver(out)
	if sum := m["a"].TargetW + m["b"].TargetW; math.Abs(sum+960) > 1 {
		t.Errorf("blocked packs keep %.0f W + %.0f W, want -960 W in total", m["a"].TargetW, m["b"].TargetW)
	}
}

// An unbalanced site: the total stays far under the fuse, but stopping a
// 3 kW discharge adds 4.35 A to each phase and L1 is at 15 A of 16 A. The
// pack keeps the discharge that holds L1 at 16 A.
func TestBlockedDischargeStopRespectsPhaseFuse(t *testing.T) {
	st := NewState(0, 42, "meter")
	st.MinDispatchIntervalS = 0
	st.SlewRateW = 500
	st.SiteFuseAmps = 16
	st.SiteFuseVoltage = 230
	st.SiteFusePhases = 3
	store := seedDeadbandSite(6000, []deadbandBattery{{"blk", -3000, 0.04, ""}})
	data, _ := json.Marshal(map[string]any{"l1_a": 15.0, "l2_a": 6.0, "l3_a": 5.1})
	store.Update("meter", telemetry.DerMeter, 6000, nil, data)
	out := ComputeDispatch(store, st, caps(map[string]float64{"blk": 10000}), 11040)
	got := targetsByDriver(out)["blk"].TargetW
	if l1 := 15 + (got+3000)/(230*3); l1 > 16.01 {
		t.Errorf("target %.0f W puts L1 at %.2f A on a 16 A fuse", got, l1)
	}
	if got < -2311 {
		t.Errorf("target %.0f W keeps more discharge than L1 needs (-2310 W)", got)
	}
}

// The stop holds while the pack's measured discharge decays.
func TestBlockedDischargeStaysStopped(t *testing.T) {
	st := NewState(0, 42, "meter")
	st.MinDispatchIntervalS = 0
	st.SlewRateW = 500
	live := -3000.0
	for tick := 0; tick < 6; tick++ {
		store := seedDeadbandSite(-1000-live, []deadbandBattery{{"bat", live, 0.04, ""}})
		out := ComputeDispatch(store, st, caps(map[string]float64{"bat": 10000}), 0)
		if got := targetsByDriver(out)["bat"].TargetW; got < -1 {
			t.Fatalf("tick %d: %.0f W", tick, got)
		}
		live /= 2
	}
}

// A manual self-tune request goes through the same floor.
func TestSelfTuneDoesNotDischargeBlockedPack(t *testing.T) {
	st := NewState(0, 42, "meter")
	store := seedDeadbandSite(2000, []deadbandBattery{{"blk", -3000, 0.04, ""}})
	out := SelfTuneDispatch(store, st, caps(map[string]float64{"blk": 10000}), 11040, "blk", -2000)
	if got := targetsByDriver(out)["blk"].TargetW; got < -1 {
		t.Errorf("self-tune sent %.0f W", got)
	}
}

// The phase relief uses only live readings from the same sample as the rest
// of the decision. A battery or meter that went offline, or a meter that
// moved on to a newer reading, gives no phase relief.
func TestStopPhaseReliefNeedsLiveReadings(t *testing.T) {
	st := NewState(0, 42, "meter")
	st.SiteFuseAmps = 16
	st.SiteFuseVoltage = 230
	st.SiteFusePhases = 3
	data, _ := json.Marshal(map[string]any{"l1_a": 15.0, "l2_a": 6.0, "l3_a": 5.1})
	seed := func() (*telemetry.Store, map[string]*telemetry.DerReading, *telemetry.DerReading) {
		store := seedDeadbandSite(6000, []deadbandBattery{{"blk", -3000, 0.04, ""}})
		store.Update("meter", telemetry.DerMeter, 6000, nil, data)
		readings := map[string]*telemetry.DerReading{"blk": store.Get("blk", telemetry.DerBattery)}
		return store, readings, store.Get("meter", telemetry.DerMeter)
	}
	stop := []DispatchTarget{{Driver: "blk", TargetW: 0}}

	store, readings, meter := seed()
	// Stopping 3000 W puts L1 at 15 + 4.35 A; 3.35 A over 16 A is 2310 W.
	if got := stopPhaseReliefW(store, stop, readings, meter, st); math.Abs(got-2310) > 1 {
		t.Fatalf("live readings: relief %.0f W, want 2310 W", got)
	}
	store.DriverHealthMut("blk").SetOffline()
	if got := stopPhaseReliefW(store, stop, readings, meter, st); got != 0 {
		t.Errorf("offline battery: relief %.0f W, want 0", got)
	}

	store, readings, meter = seed()
	store.DriverHealthMut("meter").SetOffline()
	if got := stopPhaseReliefW(store, stop, readings, meter, st); got != 0 {
		t.Errorf("offline meter: relief %.0f W, want 0", got)
	}

	store, readings, meter = seed()
	time.Sleep(time.Millisecond)
	store.Update("meter", telemetry.DerMeter, 6000, nil, data)
	if got := stopPhaseReliefW(store, stop, readings, meter, st); got != 0 {
		t.Errorf("newer meter reading: relief %.0f W, want 0", got)
	}
}
