package mpc

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// FTW #1231: health and power freshness are separate facts.
func TestBeta4AuditLiveLoadRejectsOldMeterPower(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, 4000, nil, nil)
	// Driver still answers, but has stopped updating the required power field.
	store.Get("meter", telemetry.DerMeter).UpdatedAt = time.Now().Add(-10 * time.Minute)
	store.DriverHealthMut("meter").RecordSuccess()
	s := &Service{Tele: store, SiteMeter: "meter"}
	if w, ok := s.liveHouseLoadW(); ok {
		t.Fatalf("ten-minute-old meter power accepted as live: %g W", w)
	}
}

func TestBeta4AuditLiveLoadRejectsIncompleteBalance(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, 5000, nil, nil)
	store.Update("battery", telemetry.DerBattery, 4500, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	store.DriverHealthMut("battery").SetOffline()
	s := &Service{Tele: store, SiteMeter: "meter"}
	if w, ok := s.liveHouseLoadW(); ok {
		t.Fatalf("unknown battery power converted to known household load: %g W (last complete balance 500 W)", w)
	}
}

func TestBeta4AuditLiveLoadWaitsForConfiguredFlowAndRecovers(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, 5000, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	s := &Service{Tele: store, SiteMeter: "meter"}
	s.HouseholdMeasurement = func() telemetry.ForecastReading {
		return store.ForecastMeasurementNow("meter", telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "charger", DerType: telemetry.DerEV}}})
	}
	if _, ok := s.liveHouseLoadW(); ok {
		t.Fatal("never-emitted configured EV treated as zero")
	}
	store.Update("charger", telemetry.DerEV, 4500, nil, nil)
	store.DriverHealthMut("charger").RecordSuccess()
	if w, ok := s.liveHouseLoadW(); !ok || w != 500 {
		t.Fatalf("complete balance did not recover: %g %v", w, ok)
	}
	store.Get("charger", telemetry.DerEV).UpdatedAt = time.Now().Add(-10 * time.Minute)
	if _, ok := s.liveHouseLoadW(); ok {
		t.Fatal("stale EV was subtracted from fresh meter")
	}
}
