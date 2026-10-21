package control

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func TestTwoPhaseFuseCurrentAndDemand(t *testing.T) {
	s := &State{SiteFuseVoltage: 230, SiteFusePhases: 3}
	if got := s.offerPhaseA(4600, 2); got != 10 {
		t.Fatalf("two-phase 4600 W offer = %v A, want 10 A", got)
	}
	s.SetEVFuseDemands([]loadpoint.FuseDemand{{DriverName: "easee", PowerW: 4600, PhaseMode: "2p"}})
	if _, phases := s.evNeedW(time.Now()); phases != 2 {
		t.Fatalf("two-phase demand became %v phases", phases)
	}
	pool, plugged := evPoolFor(s, []loadpoint.State{{ID: "garage", PluggedIn: true}}, []loadpoint.Config{{ID: "garage", PhaseMode: "2p", MinChargeW: 2760}})
	if !plugged || pool.phases != 2 || pool.minW != 2760 {
		t.Fatalf("two-phase pool = %+v", pool)
	}
	// The pair of phases is unknown. A two-phase cut must not claim
	// relief on all three phases, which could spend room on the third.
	s.SetEVCommanded([]loadpoint.State{{ID: "garage", DriverName: "easee", PluggedIn: true, CommandedKnown: true, CommandedW: 4600, CommandedPhases: 2}})
	for _, sent := range s.evLimit.offers["garage"].log {
		if sent.three || sent.a != 10 {
			t.Fatalf("two-phase offer credited a third phase: %+v", sent)
		}
	}
}
