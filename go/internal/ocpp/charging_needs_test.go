package ocpp

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	ocpp201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	smartcharging201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func intp(v int) *int { return &v }

// nearly compares derived fractions, which come out of a division and so do
// not land on the exact decimal the case is written with.
func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// connectStationForNeeds is connectStationV201 with the station itself handed
// back, so a test can send station-initiated messages rather than only receive
// the ones the CSMS pushes.
func connectStationForNeeds(t *testing.T, srv *Server, port int, id string) ocpp201.ChargingStation {
	t.Helper()
	cs := ocpp201.NewChargingStation(id, nil, nil)
	cs.SetSmartChargingHandler(newFakeStationV201())

	if err := cs.Start(fmt.Sprintf("ws://127.0.0.1:%d", port)); err != nil {
		t.Fatalf("charging station connect: %v", err)
	}
	var once sync.Once
	t.Cleanup(func() { once.Do(cs.Stop) })

	if _, err := cs.BootNotification(provisioning.BootReasonPowerUp, "Dawn", "Charge Amps"); err != nil {
		t.Fatalf("boot: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Handler().IsOnline(id) {
			return cs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never registered station %s as online", id)
	return nil
}

// The wire speaks Wh and whole percent; core speaks Wh and 0-1 fractions.
// A DC report carries every field, so this is where a scale slip would show.
func TestChargingNeedsFromDCConvertsUnits(t *testing.T) {
	departure := time.Date(2026, 8, 29, 7, 30, 0, 0, time.UTC)
	req := smartcharging201.NewNotifyEVChargingNeedsRequest(1, smartcharging201.ChargingNeeds{
		RequestedEnergyTransfer: smartcharging201.EnergyTransferModeDC,
		DepartureTime:           types201.NewDateTime(departure),
		DCChargingParameters: &smartcharging201.DCChargingParameters{
			EVMaxCurrent:     125,
			EVMaxVoltage:     400,
			EnergyAmount:     intp(30000),
			EVMaxPower:       intp(50000),
			StateOfCharge:    intp(20),
			EVEnergyCapacity: intp(75000),
			FullSoC:          intp(90),
		},
	})

	now := time.Date(2026, 8, 29, 6, 0, 0, 0, time.UTC)
	n := chargingNeedsFrom(req, now)

	if n.TransferMode != "DC" {
		t.Errorf("transfer mode: got %q, want DC", n.TransferMode)
	}
	if n.EnergyWh != 30000 {
		t.Errorf("energy: got %v Wh, want 30000", n.EnergyWh)
	}
	if n.CapacityWh != 75000 {
		t.Errorf("capacity: got %v Wh, want 75000", n.CapacityWh)
	}
	if n.MaxPowerW != 50000 {
		t.Errorf("max power: got %v W, want 50000", n.MaxPowerW)
	}
	if n.PresentSoC == nil || *n.PresentSoC != 0.20 {
		t.Errorf("present SoC: got %v, want the 0-1 fraction 0.20", n.PresentSoC)
	}
	if n.FullSoC == nil || *n.FullSoC != 0.90 {
		t.Errorf("full SoC: got %v, want the 0-1 fraction 0.90", n.FullSoC)
	}
	if !n.DepartureTime.Equal(departure) {
		t.Errorf("departure: got %v, want %v", n.DepartureTime, departure)
	}
	if !n.ReceivedAt.Equal(now) {
		t.Errorf("received at: got %v, want %v", n.ReceivedAt, now)
	}
	if n.EVSEID != 1 {
		t.Errorf("evse: got %d, want 1", n.EVSEID)
	}
}

// AC states energy and current, and has no SoC or capacity at all.
func TestChargingNeedsFromACHasNoSoC(t *testing.T) {
	req := smartcharging201.NewNotifyEVChargingNeedsRequest(1, smartcharging201.ChargingNeeds{
		RequestedEnergyTransfer: smartcharging201.EnergyTransferModeAC3Phase,
		ACChargingParameters: &smartcharging201.ACChargingParameters{
			EnergyAmount: 12000,
			EVMinCurrent: 6,
			EVMaxCurrent: 16,
			EVMaxVoltage: 230,
		},
	})

	n := chargingNeedsFrom(req, time.Now())

	if n.EnergyWh != 12000 {
		t.Errorf("energy: got %v Wh, want 12000", n.EnergyWh)
	}
	if n.MaxCurrentA != 16 {
		t.Errorf("max current: got %v A, want 16", n.MaxCurrentA)
	}
	if n.PresentSoC != nil || n.CapacityWh != 0 {
		t.Errorf("AC carries no SoC or capacity, got soc=%v capacity=%v", n.PresentSoC, n.CapacityWh)
	}
	if _, ok := n.TargetSoC(); ok {
		t.Error("derived a target SoC from energy alone — a fraction of an unknown battery")
	}
}

func TestChargingNeedsTargetSoC(t *testing.T) {
	soc := func(f float64) *float64 { return &f }

	tests := []struct {
		name  string
		needs ChargingNeeds
		want  float64
		ok    bool
	}{
		{
			name:  "present plus requested over capacity",
			needs: ChargingNeeds{PresentSoC: soc(0.20), EnergyWh: 30000, CapacityWh: 75000},
			want:  0.60,
			ok:    true,
		},
		{
			name:  "capped at the car's own full SoC",
			needs: ChargingNeeds{PresentSoC: soc(0.50), EnergyWh: 60000, CapacityWh: 75000, FullSoC: soc(0.80)},
			want:  0.80,
			ok:    true,
		},
		{
			name:  "capped at 1 when the car named no full SoC",
			needs: ChargingNeeds{PresentSoC: soc(0.50), EnergyWh: 60000, CapacityWh: 75000},
			want:  1.0,
			ok:    true,
		},
		{
			name:  "no capacity means no fraction",
			needs: ChargingNeeds{PresentSoC: soc(0.20), EnergyWh: 30000},
			ok:    false,
		},
		{
			name:  "no present SoC means no fraction",
			needs: ChargingNeeds{EnergyWh: 30000, CapacityWh: 75000},
			ok:    false,
		},
		{
			name:  "no energy request means nothing to add",
			needs: ChargingNeeds{PresentSoC: soc(0.20), CapacityWh: 75000},
			ok:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.needs.TargetSoC()
			if ok != tc.ok {
				t.Fatalf("ok: got %v, want %v", ok, tc.ok)
			}
			if ok && !nearly(got, tc.want) {
				t.Errorf("target SoC: got %v, want %v", got, tc.want)
			}
		})
	}
}

// Quarantine covers charging needs like everything else: a charge point no
// loadpoint names may state what it wants, and nothing acts on it.
func TestPendingChargerChargingNeedsNeverReachLoadpoint(t *testing.T) {
	h := NewHandler(telemetry.NewStore(), 60)
	h.SetApprovedIDs([]string{"garage"})

	var fired []string
	h.SetChargingNeeds(func(chargerID string, _ ChargingNeeds) {
		fired = append(fired, chargerID)
	})

	needs := ChargingNeeds{TransferMode: "DC", EnergyWh: 30000}
	h.noteChargingNeeds("intruder", needs)
	h.noteChargingNeeds("garage", needs)

	if len(fired) != 1 || fired[0] != "garage" {
		t.Fatalf("callback fired for %v, want only the adopted charger", fired)
	}
	// Recorded either way — the operator has to be able to see what asked.
	if _, ok := h.ChargingNeeds("intruder"); !ok {
		t.Error("a pending charger's needs should still be visible in the API")
	}
}

// The car may revise what it wants mid-session, so every report fires —
// unlike a vehicle identity, which fires only when it changes.
func TestChargingNeedsFireOnEveryReport(t *testing.T) {
	h := NewHandler(telemetry.NewStore(), 60)
	h.SetApprovedIDs([]string{"garage"})

	var got []float64
	h.SetChargingNeeds(func(_ string, n ChargingNeeds) {
		got = append(got, n.EnergyWh)
	})

	h.noteChargingNeeds("garage", ChargingNeeds{EnergyWh: 30000})
	h.noteChargingNeeds("garage", ChargingNeeds{EnergyWh: 30000})
	h.noteChargingNeeds("garage", ChargingNeeds{EnergyWh: 18000})

	if len(got) != 3 {
		t.Fatalf("fired %d times for 3 reports: %v", len(got), got)
	}
	last, ok := h.ChargingNeeds("garage")
	if !ok || last.EnergyWh != 18000 {
		t.Errorf("stored needs: got %v ok=%v, want the latest 18000 Wh", last.EnergyWh, ok)
	}
}

// End to end over a real 2.0.1 connection. Without SetSmartChargingHandler the
// CSMS rejects the message as unsupported, so this is what proves the handler
// is actually registered rather than merely written.
func TestV201StationReportsChargingNeeds(t *testing.T) {
	_, portV201, srv := startDualServer(t, telemetry.NewStore())
	srv.Handler().SetApprovedIDs([]string{"garage-needs"})

	applied := make(chan ChargingNeeds, 1)
	srv.Handler().SetChargingNeeds(func(_ string, n ChargingNeeds) {
		select {
		case applied <- n:
		default:
		}
	})

	station := connectStationForNeeds(t, srv, portV201, "garage-needs")

	departure := time.Now().Add(8 * time.Hour).UTC().Truncate(time.Second)
	resp, err := station.NotifyEVChargingNeeds(1, smartcharging201.ChargingNeeds{
		RequestedEnergyTransfer: smartcharging201.EnergyTransferModeDC,
		DepartureTime:           types201.NewDateTime(departure),
		DCChargingParameters: &smartcharging201.DCChargingParameters{
			EVMaxCurrent:     125,
			EVMaxVoltage:     400,
			EnergyAmount:     intp(30000),
			StateOfCharge:    intp(20),
			EVEnergyCapacity: intp(75000),
		},
	})
	if err != nil {
		t.Fatalf("NotifyEVChargingNeeds: %v", err)
	}
	if resp.Status != smartcharging201.EVChargingNeedsStatusAccepted {
		t.Fatalf("status: got %v, want Accepted", resp.Status)
	}

	select {
	case n := <-applied:
		if n.CapacityWh != 75000 {
			t.Errorf("capacity: got %v Wh, want 75000", n.CapacityWh)
		}
		target, ok := n.TargetSoC()
		if !ok || !nearly(target, 0.60) {
			t.Errorf("target SoC: got %v ok=%v, want 0.60", target, ok)
		}
		if !n.DepartureTime.Equal(departure) {
			t.Errorf("departure: got %v, want %v", n.DepartureTime, departure)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("charging needs never reached the loadpoint callback")
	}

	// And it is visible in the snapshot the Chargers panel renders.
	view, ok := srv.Handler().Snapshot()["garage-needs"]
	if !ok || view.ChargingNeeds == nil {
		t.Fatal("snapshot carried no charging needs")
	}
	if view.ChargingNeeds.EnergyWh != 30000 {
		t.Errorf("snapshot energy: got %v Wh, want 30000", view.ChargingNeeds.EnergyWh)
	}
}
