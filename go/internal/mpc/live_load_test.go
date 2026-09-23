package mpc

import (
	"math"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestOverlayLiveHouseLoadOnCurrentSlot(t *testing.T) {
	slots := []Slot{{LoadW: 3987}, {LoadW: 2470}}
	overlayLiveHouseLoad(slots, 480, true)
	if math.Abs(slots[0].LoadW-480) > 1e-9 {
		t.Fatalf("current slot load = %v, want live 480", slots[0].LoadW)
	}
	if slots[1].LoadW != 2470 {
		t.Fatalf("later slot was painted with now: %v", slots[1].LoadW)
	}
}

func TestOverlayLiveHouseLoadKeepsModelWhenMeterStale(t *testing.T) {
	slots := []Slot{{LoadW: 3987}}
	overlayLiveHouseLoad(slots, 480, false)
	if slots[0].LoadW != 3987 {
		t.Fatalf("stale meter overwrote the model: %v", slots[0].LoadW)
	}
}

func TestOverlayLiveHouseLoadDoesNotGoNegative(t *testing.T) {
	slots := []Slot{{LoadW: 4000}}
	overlayLiveHouseLoad(slots, -200, true)
	if slots[0].LoadW != 0 {
		t.Fatalf("negative overlay = %v, want 0", slots[0].LoadW)
	}
}

func TestLiveHouseLoadWMatchesHouseOnlyIdentity(t *testing.T) {
	store := telemetry.NewStore()
	soc := 0.5
	store.Update("meter", telemetry.DerMeter, 100, nil, nil)
	store.Update("pv", telemetry.DerPV, -800, nil, nil)
	store.Update("sungrow", telemetry.DerBattery, 200, &soc, nil)
	store.Update("easee", telemetry.DerEV, 0, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	store.DriverHealthMut("pv").RecordSuccess()
	store.DriverHealthMut("sungrow").RecordSuccess()
	store.DriverHealthMut("easee").RecordSuccess()
	s := &Service{Tele: store, SiteMeter: "meter"}
	got, ok := s.liveHouseLoadW()
	if !ok {
		t.Fatal("live meter reported missing")
	}
	// grid 100 − pv (−800) − bat 200 − ev 0 = 700
	if math.Abs(got-700) > 1e-9 {
		t.Fatalf("house load = %v, want 700", got)
	}
	store.DriverHealthMut("meter").SetOffline()
	if _, ok := s.liveHouseLoadW(); ok {
		t.Fatal("offline meter still overlaid")
	}
}
