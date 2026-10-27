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
	for clk.Sub(start) < 38*time.Second {
		store.Update("meter", telemetry.DerMeter, 2667, nil, frozenRigMeter)
		store.DriverHealthMut("meter").RecordSuccess()
		st.ObserveSiteMeterFreeze(store, clk)
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
		t.Fatalf("meter repeated one sample for 38 s while battery orders moved %.0f..%.0f W; not stale", minW, maxW)
	}
	if frozenAfter >= 38*time.Second {
		t.Fatalf("stale only after %v; the rig's EV rise went out at 38 s", frozenAfter)
	}
	t.Logf("stale after %v; battery orders %.0f..%.0f W", frozenAfter, minW, maxW)

	// Any change in the meter's values ends the freeze.
	store.Update("meter", telemetry.DerMeter, 2654, nil, nil)
	st.ObserveSiteMeterFreeze(store, clk)
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
		st.ObserveSiteMeterFreeze(store, clk)
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
		st.ObserveSiteMeterFreeze(store, clk)
		if st.SiteMeterFrozen() {
			t.Fatalf("10 s telegram meter marked stale at %d s", s)
		}
		clk = clk.Add(time.Second)
	}
}

// Orders a device does not follow do not count: a car that does not draw a
// raised offer (a slow charger, or a full car), a full battery that stays
// at 0 W, and a battery that charges on its own against an order to
// discharge. The rig's meter has no noise, so it repeats itself here.
func TestFrozenMeterOrdersNotFollowedDoNotCount(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk)
	st.PrevTargets["bat"] = 0
	store.Update("bat", telemetry.DerBattery, 0, ptrF64(1), nil)
	store.Update("own", telemetry.DerBattery, 0, ptrF64(0.1), nil)
	store.Update("charger", telemetry.DerEV, 0, nil, nil)
	for i := 0; i < 60; i++ { // 2 min at 2 s
		st.PrevTargets["bat"] = 3000 * float64(i%2)
		st.evCommanded = map[string]float64{"charger": 11040}
		if i == 1 {
			st.PrevTargets["own"] = -3000
			store.Update("own", telemetry.DerBattery, 1000, ptrF64(0.1), nil)
		}
		store.Update("meter", telemetry.DerMeter, -2332, nil, nil)
		st.ObserveSiteMeterFreeze(store, clk)
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
		st.ObserveSiteMeterFreeze(store, clk)
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

// A 10 s HAN or DSMR 4 meter behind a bridge polled every second that loses
// one telegram (a CRC error) serves the old values for 20 s. A battery move
// right after the last good telegram must not mark this live meter stale.
func TestFrozenMeterLostTelegramDoesNotTrip(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk)
	tripped := -1
	gridW := 800.0
	for s := 0; s < 60; s++ {
		// the battery steps 1 kW further one second after each telegram
		if s%10 == 1 {
			setBattery(st, store, -float64(s/10)*1000)
		}
		// telegrams every 10 s, except the one at s=20 is lost
		if s%10 == 0 && s != 20 {
			gridW++
		}
		store.Update("meter", telemetry.DerMeter, gridW, nil, nil)
		st.ObserveSiteMeterFreeze(store, clk)
		if st.SiteMeterFrozen() && tripped < 0 {
			tripped = s
		}
		clk = clk.Add(time.Second)
	}
	if tripped >= 0 {
		t.Errorf("live 10 s meter with one lost telegram marked stale at %d s", tripped)
	}
}

// Two batteries trade 1.5 kW each way. The meter sees only the sum, which
// does not change, so the repeat is no evidence.
func TestFrozenMeterOppositeMovesCancel(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk)
	set := func(name string, w float64) {
		st.PrevTargets[name] = w
		store.Update(name, telemetry.DerBattery, w, ptrF64(0.5), nil)
	}
	set("a", 2000)
	set("b", 0)
	for s := 0; s < 60; s++ {
		if s == 2 {
			set("a", 500)
			set("b", 1500)
		}
		store.Update("meter", telemetry.DerMeter, 300, nil, nil) // net unchanged
		st.ObserveSiteMeterFreeze(store, clk)
		if st.SiteMeterFrozen() {
			t.Errorf("opposite moves that cancel at the meter marked it stale at %d s", s)
			return
		}
		clk = clk.Add(time.Second)
	}
}

// A car plugged in during a freeze, at a site with no battery: its charger
// had no offer when the meter last changed. FTW's raise, which the car
// draws, must still count, or an EV rise on a frozen meter goes unseen.
func TestFrozenMeterNewlyPluggedEVCounts(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk)
	store.Update("charger", telemetry.DerEV, 0, nil, nil)
	for s := 0; s < 60; s++ {
		switch {
		case s == 5: // car plugged: first offer, 0 W
			st.evCommanded = map[string]float64{"charger": 0}
		case s >= 10: // FTW raises to 6 A x 3 and the car draws it
			st.evCommanded = map[string]float64{"charger": 4140}
			store.Update("charger", telemetry.DerEV, 4140, nil, nil)
		}
		store.Update("meter", telemetry.DerMeter, 2667, nil, frozenRigMeter)
		st.ObserveSiteMeterFreeze(store, clk)
		if st.SiteMeterFrozen() {
			return
		}
		clk = clk.Add(time.Second)
	}
	t.Error("frozen meter while FTW raised a newly plugged EV 4.1 kW for 50 s: never stale")
}

// A battery that moves 2 kW and back before the meter's next telegram
// leaves nothing for the meter to show, so a repeat is no evidence.
func TestFrozenMeterMoveAndBackDoesNotCount(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk)
	setBattery(st, store, 0)
	for s := 0; s < 90; s++ {
		switch s {
		case 1:
			setBattery(st, store, 2000)
		case 5:
			setBattery(st, store, 0)
		}
		store.Update("meter", telemetry.DerMeter, 300, nil, nil)
		st.ObserveSiteMeterFreeze(store, clk)
		if st.SiteMeterFrozen() {
			t.Fatalf("a move that came back marked the meter stale at %d s", s)
		}
		clk = clk.Add(time.Second)
	}
}

// A meter already frozen when FTW starts: FTW has no battery orders yet
// when the meter's only sample is taken. Winding the battery 3 kW on it must
// still count.
func TestFrozenMeterFromStartIsCaught(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk) // PrevTargets empty, as after a restart
	store.Update("bat", telemetry.DerBattery, 0, ptrF64(0.5), nil)
	for s := 0; s < 60; s++ {
		if s >= 3 {
			setBattery(st, store, -3000)
		}
		store.Update("meter", telemetry.DerMeter, 2667, nil, frozenRigMeter)
		st.ObserveSiteMeterFreeze(store, clk)
		if st.SiteMeterFrozen() {
			return
		}
		clk = clk.Add(time.Second)
	}
	t.Error("meter frozen from boot while FTW wound the battery to -3 kW: never stale")
}

// After a start, FTW's first order to a battery that already runs on its
// own counts only by how far the battery's reading moves. Ordering what it
// already does is no move.
func TestFrozenMeterFirstOrderMatchingAutonomousBatteryDoesNotCount(t *testing.T) {
	store := telemetry.NewStore()
	clk := time.Now()
	st := freezeState(&clk)
	store.Update("bat", telemetry.DerBattery, 1500, ptrF64(0.5), nil)
	for s := 0; s < 60; s++ {
		if s >= 3 {
			setBattery(st, store, 1500)
		}
		store.Update("meter", telemetry.DerMeter, 2667, nil, frozenRigMeter)
		st.ObserveSiteMeterFreeze(store, clk)
		if st.SiteMeterFrozen() {
			t.Fatalf("first order matching the battery's own 1500 W marked the meter stale at %d s", s)
		}
		clk = clk.Add(time.Second)
	}
}
