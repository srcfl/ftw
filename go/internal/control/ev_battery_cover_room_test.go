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
