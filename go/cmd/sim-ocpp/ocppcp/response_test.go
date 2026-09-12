package ocppcp

import (
	"math"
	"testing"
	"time"

	ocpp201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

type eventRecorder struct {
	ocpp201.ChargingStation
	sequences []int
}

func (r *eventRecorder) TransactionEvent(_ transactions.TransactionEvent, _ *types201.DateTime, _ transactions.TriggerReason, seq int, _ transactions.Transaction, _ ...func(*transactions.TransactionEventRequest)) (*transactions.TransactionEventResponse, error) {
	r.sequences = append(r.sequences, seq)
	return &transactions.TransactionEventResponse{}, nil
}

func TestTransactionEventsAdvanceSequence(t *testing.T) {
	model, _ := Lookup("defa-power")
	sim := New(model)
	recorder := &eventRecorder{}
	sim.cs = recorder
	for _, send := range []func() error{sim.startTx, sim.Report, sim.Report, sim.stopTx} {
		if err := send(); err != nil {
			t.Fatal(err)
		}
	}
	if len(recorder.sequences) != 4 {
		t.Fatal(recorder.sequences)
	}
	for i := 1; i < len(recorder.sequences); i++ {
		if recorder.sequences[i] != recorder.sequences[i-1]+1 {
			t.Fatalf("transaction event sequences must advance once: %v", recorder.sequences)
		}
	}
}

func TestProfileResponsePreservesLag(t *testing.T) {
	model, _ := Lookup("easee-charge-up")
	sim := New(model)
	sim.physics = newPhysics(model, 0.5)
	sim.physics.Plugged = true
	energy := sim.physics.EnergyWh
	sim.recordAttempt(ProfileAttempt{Applied: true, LimitA: 10})
	if sim.PowerW() != 0 || sim.physics.EnergyWh != energy {
		t.Fatal("accepting a profile advanced physics without elapsed time")
	}
	sim.Tick(500 * time.Millisecond)
	want := 10 * (1 - math.Exp(-1)) * SiteVoltage * 3
	if math.Abs(sim.PowerW()-want) > 0.001 {
		t.Fatalf("power=%v, want %v", sim.PowerW(), want)
	}
	before := sim.PowerW()
	sim.recordAttempt(ProfileAttempt{Applied: true, LimitA: 0})
	if sim.PowerW() != before {
		t.Fatal("pause bypassed the configured response lag")
	}
	sim.Tick(500 * time.Millisecond)
	if sim.PowerW() <= 0 || sim.PowerW() >= before {
		t.Fatal("pause did not approach zero")
	}
}
