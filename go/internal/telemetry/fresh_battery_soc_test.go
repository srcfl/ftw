package telemetry

import (
	"testing"
	"time"
)

func TestFreshBatterySoCOnline(t *testing.T) {
	s := NewStore()
	now := time.Now()
	if s.FreshBatterySoCOnline(now) {
		t.Fatal("fresh with no battery at all")
	}
	soc := 0.5
	s.Update("pv", DerPV, -1200, nil, nil)
	s.RecordDriverSuccess("pv")
	s.Update("battery", DerBattery, 0, &soc, nil)
	if s.FreshBatterySoCOnline(time.Now()) {
		t.Fatal("fresh before the driver has health")
	}
	s.RecordDriverSuccess("battery")
	if !s.FreshBatterySoCOnline(time.Now()) {
		t.Fatal("not fresh with an online battery and a new charge level")
	}
	if s.FreshBatterySoCOnline(time.Now().Add(BatterySoCMaxAge + time.Minute)) {
		t.Fatal("fresh on a charge level older than the planner accepts")
	}
	s.DriverHealthMut("battery").SetOffline()
	if s.FreshBatterySoCOnline(time.Now()) {
		t.Fatal("fresh from an offline driver")
	}
}

func TestFreshBatterySoCOnlineSkipsCalibrating(t *testing.T) {
	s := NewStore()
	soc := 0.5
	s.Update("battery", DerBattery, 0, &soc, nil)
	s.RecordDriverSuccess("battery")
	s.SetDriverCalibrating("battery", true)
	if s.FreshBatterySoCOnline(time.Now()) {
		t.Fatal("fresh from a calibrating driver the planner skips")
	}
}
