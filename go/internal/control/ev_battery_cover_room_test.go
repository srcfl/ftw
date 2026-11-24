package control

import (
	"fmt"
	"testing"
	"time"
)

// A field night, 15 Nov 2026: the car charged against the fuse while the
// plan discharged the battery to cover the house. The meter showed the
// room the discharge made, and the car took it, so about two thirds of the
// battery's energy went into the car although the battery could cover only
// the house. A battery that may not cover the car leaves the car the room
// the grid gives with the battery idle. A battery that may cover it still
// lends it the room.
func TestPlannedDischargeIsRoomOnlyForACarTheBatteryMayCover(t *testing.T) {
	const houseW, lineW = 10000.0, 16560.0 // 24 A on three phases
	for _, covers := range []bool{false, true} {
		t.Run(fmt.Sprintf("covers=%v", covers), func(t *testing.T) {
			s := newEVBatterySite(t, houseW)
			s.state.BatteryCoversEV = covers
			start := s.now
			discharge := SlotDirective{SlotStart: start, SlotEnd: start.Add(time.Hour), BatteryEnergyWh: -4000, Strategy: "arbitrage"}
			s.state.SlotDirective = func(time.Time) (SlotDirective, bool) { return discharge, true }
			s.run(10 * time.Minute)
			t.Logf("offer %.0f W, battery %.0f W, grid %.0f W, highest grid %.0f W", s.offer(), s.batteryW, s.gridW(), s.maxGridW)
			if s.batteryW > -3000 {
				t.Fatalf("setup: the plan's discharge did not run: battery %.0f W", s.batteryW)
			}
			if s.maxGridW > lineW+1e-6 {
				t.Fatalf("grid %.0f W over the %v W line", s.maxGridW, lineW)
			}
			gridRoomW := lineW - houseW
			if !covers && s.offer() > gridRoomW {
				t.Fatalf("the car took %.0f W, past the grid's %.0f W of room: the battery's discharge went into the car", s.offer(), gridRoomW)
			}
			if covers && s.offer() <= gridRoomW {
				t.Fatalf("a battery that may cover the car left it at %.0f W, inside the grid's %.0f W of room", s.offer(), gridRoomW)
			}
		})
	}
}

// A boost lets the battery help the car, so the room its discharge makes is
// the car's from the start. The lease's live draw is zero until the car
// starts, so the lease itself counts: with the house near the line and the
// battery covering it, a boosted car that waited for its own draw would
// never start. Without a boost the car waits for the grid's own room.
func TestABoostedCarStartsOnTheBatterysRoom(t *testing.T) {
	const houseW, lineW, minW = 13000.0, 16560.0, 4140.0 // grid room 3,560 W
	for _, boosted := range []bool{false, true} {
		t.Run(fmt.Sprintf("boosted=%v", boosted), func(t *testing.T) {
			s := newEVBatterySite(t, houseW)
			s.state.BatteryBoostActive = boosted
			discharge := SlotDirective{SlotStart: s.now, SlotEnd: s.now.Add(time.Hour), BatteryEnergyWh: -4000, Strategy: "arbitrage"}
			s.state.SlotDirective = func(time.Time) (SlotDirective, bool) { return discharge, true }
			s.run(10 * time.Minute)
			t.Logf("offer %.0f W, battery %.0f W, grid %.0f W, highest grid %.0f W", s.offer(), s.batteryW, s.gridW(), s.maxGridW)
			if s.batteryW > -3000 {
				t.Fatalf("setup: the plan's discharge did not run: battery %.0f W", s.batteryW)
			}
			if s.maxGridW > lineW+1e-6 {
				t.Fatalf("grid %.0f W over the %v W line", s.maxGridW, lineW)
			}
			if boosted && s.offer() < minW {
				t.Fatalf("a boosted car stayed at %.0f W beside %.0f W of battery discharge", s.offer(), -s.batteryW)
			}
			if !boosted && s.offer() > 0 {
				t.Fatalf("the car started at %.0f W on the battery's room, with %.0f W of grid room", s.offer(), lineW-houseW)
			}
		})
	}
}
