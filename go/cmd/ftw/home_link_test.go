package main

import (
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/homelink"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestHomeLinkOverviewUsesOnlyOnlineSiteReadings(t *testing.T) {
	tel := telemetry.NewStore()
	for _, name := range []string{"meter", "solar", "battery", "stale-solar"} {
		tel.EnsureDriverHealth(name)
	}
	soc := 0.5
	tel.Update("meter", telemetry.DerMeter, 1000, nil, nil)
	tel.Update("solar", telemetry.DerPV, -300, nil, nil)
	tel.Update("battery", telemetry.DerBattery, -200, &soc, nil)
	tel.Update("stale-solar", telemetry.DerPV, -5000, nil, nil)
	for _, name := range []string{"meter", "solar", "battery"} {
		tel.RecordDriverSuccess(name)
	}
	tel.DriverHealthMut("stale-solar").SetOffline()

	ctrl := &control.State{
		Mode: control.ModeIdle, SiteMeterDriver: "meter",
	}
	overview, err := homeLinkOverview(nil, tel, ctrl, &sync.Mutex{})
	if err != nil {
		t.Fatal(err)
	}
	if !overview.GridAvailable || overview.GridW != 1000 ||
		overview.PVW != -300 || overview.BatW != -200 ||
		overview.LoadW != 1500 {
		t.Fatalf("overview power = %+v", overview)
	}
	if !overview.BatSoCAvailable || overview.BatSoC != 0.5 ||
		overview.Mode != string(control.ModeIdle) {
		t.Fatalf("overview state = %+v", overview)
	}
	response := homelink.ReadResponse{
		Version: homelink.ReadContractVersion, Scope: homelink.ScopeOverviewRead,
		Overview: &overview,
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("overview validation: %v", err)
	}
}

func TestHomeLinkOverviewMarksOfflineSiteMeterUnavailable(t *testing.T) {
	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("meter")
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	tel.DriverHealthMut("meter").SetOffline()
	ctrl := &control.State{Mode: control.ModeIdle, SiteMeterDriver: "meter"}

	overview, err := homeLinkOverview(nil, tel, ctrl, &sync.Mutex{})
	if err != nil {
		t.Fatal(err)
	}
	if overview.GridAvailable || overview.GridW != 0 || overview.LoadW != 0 {
		t.Fatalf("offline meter overview = %+v", overview)
	}
}
