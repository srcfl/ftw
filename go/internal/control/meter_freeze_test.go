package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// frozenRigMeter is the sample the rig's site meter repeated in seed 207537.
var frozenRigMeter = json.RawMessage(`{"w":2667,"l1_a":2.4,"l2_a":5.5,"l3_a":-0.2,` +
	`"l1_w":552,"l2_w":1265,"l3_w":-46,"l1_v":230,"l2_v":230,"l3_v":230}`)

func freezeState(clk *time.Time) *State {
	st := NewState(0, 50, "meter")
	st.clock = func() time.Time { return *clk }
	return st
}

// setBattery sends an order and lets the battery follow it.
func setBattery(st *State, store *telemetry.Store, w float64) {
	st.PrevTargets["bat"] = w
	store.Update("bat", telemetry.DerBattery, w, ptrF64(0.5), nil)
}

// Seed 207537: the meter answers every poll with one sample while the
// battery PI winds on it. The freeze must count as stale well before the
// rig's EV rise went out (38 s into the freeze), and the battery must stop
// moving once it does.
func TestFrozenMeterWithMovingOrdersIsStale(t *testing.T) {
	store := telemetry.NewStore()
	soc := 0.5
	store.Update("bat", telemetry.DerBattery, 4075, &soc, nil)
	store.DriverHealthMut("bat").RecordSuccess()

	clk := time.Now()
	st := freezeState(&clk)
	st.Mode = ModeSelfConsumption
	st.MinDispatchIntervalS = 0
	st.DriverLimits = map[string]PowerLimits{"bat": {MaxChargeW: 5000, MaxDischargeW: 5000}}
	st.PrevTargets["bat"] = 4075
	capsMap := caps(map[string]float64{"bat": 10000})

	frozenAfter := time.Duration(-1)
	minW, maxW := 4075.0, 4075.0
	start := clk
	for clk.Sub(start) <= 30*time.Second {
		store.Update("meter", telemetry.DerMeter, 2667, nil, frozenRigMeter)
		store.DriverHealthMut("meter").RecordSuccess()
		st.ObserveSiteMeterFreeze(store)
		if st.SiteMeterFrozen() {
			if frozenAfter < 0 {
				frozenAfter = clk.Sub(start)
			}
		} else {
			// The gate is open: dispatch acts on the frozen meter.
			for _, tg := range ComputeDispatch(store, st, capsMap, 3*16*230) {
				if tg.Driver == "bat" {
					store.Update("bat", telemetry.DerBattery, tg.TargetW, &soc, nil)
					minW, maxW = min(minW, tg.TargetW), max(maxW, tg.TargetW)
				}
			}
		}
		clk = clk.Add(2 * time.Second)
	}
	if frozenAfter < 0 {
		t.Fatalf("meter repeated one sample for 30 s while battery orders moved %.0f..%.0f W; not stale", minW, maxW)
	}
	if frozenAfter >= 38*time.Second {
		t.Fatalf("stale only after %v; the rig's EV rise went out at 38 s", frozenAfter)
	}
	t.Logf("stale after %v; battery orders %.0f..%.0f W", frozenAfter, minW, maxW)

	// Any change in the meter's values ends the freeze.
	store.Update("meter", telemetry.DerMeter, 2654, nil, nil)
	st.ObserveSiteMeterFreeze(store)
	if st.SiteMeterFrozen() {
		t.Fatal("a moving meter stayed stale")
	}
}

// A quiet site whose devices do not move keeps a meter that repeats itself.
func TestFrozenMeterQuietSiteStaysFresh(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk)
	for i := 0; i < 150; i++ { // 5 min at 2 s
		setBattery(st, store, -300+float64(i%2)*400) // small PI moves under the threshold
		store.Update("meter", telemetry.DerMeter, 12, nil, frozenRigMeter)
		st.ObserveSiteMeterFreeze(store)
		if st.SiteMeterFrozen() {
			t.Fatalf("quiet site marked stale after %d identical samples", i+1)
		}
		clk = clk.Add(2 * time.Second)
	}
}

// A HAN or DSMR 4 meter sends a new reading every 10 s; a driver polling it
// every second repeats each reading ten times. Large battery moves inside
// that window must not mark it stale, or it would flap.
func TestFrozenMeterTenSecondTelegramsDoNotFlap(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk)
	for s := 0; s < 300; s++ {
		setBattery(st, store, float64((s%4)-2)*2000) // the battery swings +-4 kW
		gridW := 800 + float64((s/10)%7)             // one new value per 10 s telegram
		store.Update("meter", telemetry.DerMeter, gridW, nil, nil)
		st.ObserveSiteMeterFreeze(store)
		if st.SiteMeterFrozen() {
			t.Fatalf("10 s telegram meter marked stale at %d s", s)
		}
		clk = clk.Add(time.Second)
	}
}

// Orders a device does not follow do not count: a car that does not draw a
// raised offer (a slow charger, or a full car) and a full battery that
// stays at 0 W. The rig's meter has no noise, so it repeats itself here.
func TestFrozenMeterOrdersNotFollowedDoNotCount(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk)
	st.PrevTargets["bat"] = 0
	store.Update("bat", telemetry.DerBattery, 0, ptrF64(1), nil)
	store.Update("charger", telemetry.DerEV, 0, nil, nil)
	for i := 0; i < 60; i++ { // 2 min at 2 s
		st.PrevTargets["bat"] = 3000 * float64(i%2)
		st.evCommanded = map[string]float64{"charger": 11040}
		store.Update("meter", telemetry.DerMeter, -2332, nil, nil)
		st.ObserveSiteMeterFreeze(store)
		if st.SiteMeterFrozen() {
			t.Fatalf("orders the devices ignored marked the meter stale after %d samples", i+1)
		}
		clk = clk.Add(2 * time.Second)
	}
}

// The verdict waits for the slowest legitimate refresh after the move, and
// for several samples, before it calls the meter stale.
func TestFrozenMeterWaitsForTheMoveToLand(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk)
	poll := func() bool {
		store.Update("meter", telemetry.DerMeter, 500, nil, nil)
		st.ObserveSiteMeterFreeze(store)
		return st.SiteMeterFrozen()
	}
	setBattery(st, store, 0)
	poll()
	setBattery(st, store, -2000)
	poll() // the move is seen here
	moved := clk
	for clk.Sub(moved) < meterFreezeAfter-time.Second {
		clk = clk.Add(time.Second)
		if poll() {
			t.Fatalf("stale %v after the move", clk.Sub(moved))
		}
	}
	clk = clk.Add(time.Second)
	if !poll() {
		t.Fatalf("not stale %v after a 2 kW move", clk.Sub(moved))
	}
}
