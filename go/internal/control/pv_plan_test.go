package control

import (
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"testing"
	"time"
)

func pvProofState(t *testing.T) (*State, *telemetry.Store, mpc.PVCurtailment, telemetry.ForecastOptions) {
	t.Helper()
	store := telemetry.NewStore()
	emitPV(t, store, "pv", -6000)
	emitMeter(t, store, "site", -5500)
	s := NewState(0, 0, "site")
	s.SupportsPVCurtail = map[string]bool{"pv": true}
	proof := mpc.PVCurtailment{Driver: "pv", Proof: "loaded-generation-1", MinW: 2, MaxW: 15000}
	s.PVGenerationLimit = func(string) mpc.PVCurtailment { return proof }
	options := telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "pv", DerType: telemetry.DerPV}}}
	return s, store, proof, options
}

func TestPlanningPVRequiresLoadedGenerationAndCompleteDomain(t *testing.T) {
	s, store, proof, options := pvProofState(t)
	if got := PlanningPVCurtailment(s, store, options); got != proof {
		t.Fatalf("capability=%+v", got)
	}
	dir := SlotDirective{PVLimitW: 100, PVCurtailActive: true, PVCurtailment: proof}
	s.SlotDirective = stubSlotDirective(dir)
	if got := findCurtail(ComputePVCurtail(s, store)); len(got) != 1 || got["pv"] != 100 {
		t.Fatalf("cap raised by live headroom: %v", got)
	}
	s.PVGenerationLimit = nil
	if PlanningPVCurtailment(s, store, options).Valid() || PlanningPVDirectiveValid(s, store, dir) {
		t.Fatal("config opt-in was treated as command proof")
	}
	s.PVGenerationLimit = func(string) mpc.PVCurtailment { p := proof; p.Proof = "new-generation"; return p }
	if PlanningPVDirectiveValid(s, store, dir) {
		t.Fatal("old plan survived driver replacement")
	}
	s.PVGenerationLimit = func(string) mpc.PVCurtailment { return proof }
	s.clock = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if PlanningPVDirectiveValid(s, store, dir) {
		t.Fatal("stale telemetry kept plan credit")
	}
	s.clock = nil
	emitPV(t, store, "second", -1)
	options.ExpectedFlows = append(options.ExpectedFlows, telemetry.ForecastFlow{Driver: "second", DerType: telemetry.DerPV})
	if PlanningPVCurtailment(s, store, options).Valid() || PlanningPVDirectiveValid(s, store, dir) {
		t.Fatal("independent domains treated as one aggregate actuator")
	}
}

func TestPlannedPVCapKeepsManualAndProtectiveCeilings(t *testing.T) {
	s, store, proof, _ := pvProofState(t)
	emitBattery(t, store, "battery", 0, .85)
	s.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 4000, PVCurtailActive: true, PVCurtailment: proof})
	s.DCLinkProtectionEnabled = true
	s.DCLinkProtectionSoCThreshold = .8
	s.DCLinkProtectionMarginW = 1000
	if got := findCurtail(ComputePVCurtail(s, store)); got["pv"] != 1500 {
		t.Fatalf("protective cap relaxed: %v", got)
	}
	s.ManualPVHold = PVManualHold{LimitW: 50, ExpiresAt: time.Now().Add(time.Minute)}
	if got := findCurtail(ComputePVCurtail(s, store)); got["pv"] != 50 {
		t.Fatalf("manual cap relaxed: %v", got)
	}
}
