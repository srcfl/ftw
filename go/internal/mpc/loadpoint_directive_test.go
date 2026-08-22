package mpc

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func TestSlotDirectiveLoadpointDirectiveCarriesEVBudget(t *testing.T) {
	start := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	d := SlotDirective{
		SlotStart:         start,
		SlotEnd:           start.Add(15 * time.Minute),
		BatteryEnergyWh:   2500,
		LoadpointEnergyWh: map[string]float64{"garage": 1035},
	}
	got := d.LoadpointDirective()
	if !got.SlotStart.Equal(d.SlotStart) || !got.SlotEnd.Equal(d.SlotEnd) {
		t.Fatalf("slot window changed: %+v", got)
	}
	if got.LoadpointEnergyWh["garage"] != 1035 {
		t.Fatalf("EV budget dropped: %+v", got.LoadpointEnergyWh)
	}
}

func TestPeakPlannedSurplusForEVSkipsGridFundedCharge(t *testing.T) {
	start := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	actions := []Action{
		{
			SlotStartMs: start.UnixMilli(), SlotLenMin: 15,
			LoadW: 500, PVW: -8000, BatteryW: 10000, GridW: 2500,
		},
		{
			SlotStartMs: start.Add(15 * time.Minute).UnixMilli(), SlotLenMin: 15,
			LoadW: 500, PVW: 0, BatteryW: 0, GridW: 500,
		},
	}
	peak, ok := PeakPlannedSurplusForEV(actions, start.Add(time.Second), 30*time.Minute)
	if !ok {
		t.Fatal("expected a peak")
	}
	want := loadpoint.PlannedSurplusForEVW(500, -8000, 10000, 2500)
	if peak != want {
		t.Errorf("peak = %.0f, want %.0f", peak, want)
	}
	if peak != 7500 {
		t.Errorf("grid-funded slot must still offer leftover PV %.0f, got %.0f", 7500.0, peak)
	}
}

func TestPeakPlannedSurplusForEVHidesSoakWhenEVMakesMeterImport(t *testing.T) {
	start := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	actions := []Action{{
		SlotStartMs: start.UnixMilli(), SlotLenMin: 15,
		LoadW: 500, PVW: -8000, BatteryW: 4000, LoadpointW: 6900,
		GridW: loadpoint.GridW(500, -8000, 4000, 6900),
	}}
	peak, ok := PeakPlannedSurplusForEV(actions, start.Add(time.Second), 30*time.Minute)
	if !ok {
		t.Fatal("expected a peak")
	}
	if peak != 3500 {
		t.Errorf("soak+EV peak = %.0f, want 3500 (leftover minus soak, not full leftover)", peak)
	}
}
