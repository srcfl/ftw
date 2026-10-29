package api

import (
	"github.com/srcfl/ftw/go/internal/state"
	"testing"
)

func TestHistoricalRepairEnergyFieldsPreserveOtherUnknownFlows(t *testing.T) {
	got := energyTodayFields(state.DayEnergy{Missing: state.MissingPV | state.EstimatedLoad, Intervals: 10, LoadWh: 500, LoadUntilMS: 1800000})
	if got["load_wh"] != float64(500) || got["load_quality"] != "estimated" || got["load_until_ms"] != int64(1800000) {
		t.Fatal(got)
	}
	if got["pv_wh"] != nil || got["history_quality"] != "incomplete" {
		t.Fatal("repair relabelled missing PV as known", got)
	}
	if got["measured"].(map[string]any)["load_wh"] != nil {
		t.Fatal("repair became a measurement", got)
	}
}

func TestEndpointSolarEnergyFieldsAreEstimatedNotMeasured(t *testing.T) {
	got := energyTodayFields(state.DayEnergy{Missing: state.EndpointEstimatedPV, Intervals: 10, PVWh: 1234, PVUntilMS: 1800000})
	if got["pv_wh"] != float64(1234) || got["pv_quality"] != "estimated" || got["pv_until_ms"] != int64(1800000) || got["history_quality"] != "estimated" {
		t.Fatal(got)
	}
	if got["measured"].(map[string]any)["pv_wh"] != nil {
		t.Fatal("estimated solar labelled measured", got)
	}
}
