package main

import (
	"github.com/srcfl/ftw/go/internal/config"
)

// plannerEngineConfig is the smallest config buildMPC will accept: a planner,
// a price provider and one battery with capacity.
func plannerEngineConfig(planner *config.Planner) (*config.Config, map[string]float64) {
	return &config.Config{
		Price:   &config.Price{Provider: "elprisetjustnu", Zone: "SE3"},
		Planner: planner,
		Drivers: []config.Driver{{Name: "sungrow", BatteryCapacityWh: 9600}},
	}, map[string]float64{
		"sungrow": 9600,
	}
}
