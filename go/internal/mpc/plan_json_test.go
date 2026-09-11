package mpc

import (
	"encoding/json"
	"math"
	"testing"
)

// The on-box Plan UI reads actions[].soc as a 0–1 fraction. A missing
// or non-finite value draws a flat 0% line. The help report prints
// Action.SoC from the same Latest() plan, so the API JSON has to keep
// the number.
func TestPlanJSONKeepsNumericActionSoC(t *testing.T) {
	plan := &Plan{
		Mode:         ModePassiveArbitrage,
		HorizonSlots: 1,
		CapacityWh:   20000,
		InitialSoC:   0.535,
		Actions: []Action{{
			SlotStartMs: 1756323000000,
			SlotLenMin:  15,
			BatteryW:    -577,
			GridW:       0,
			SoC:         0.527,
			StorageEnergyWh: map[string]float64{
				"pixii": 10540,
			},
		}},
	}
	payload := map[string]any{"enabled": true, "plan": plan}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Plan struct {
			CapacityWh float64 `json:"capacity_wh"`
			InitialSoC float64 `json:"initial_soc"`
			Actions    []struct {
				SoC             float64            `json:"soc"`
				BatteryW        float64            `json:"battery_w"`
				StorageEnergyWh map[string]float64 `json:"storage_energy_wh"`
			} `json:"actions"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Plan.CapacityWh != 20000 || got.Plan.InitialSoC != 0.535 {
		t.Fatalf("plan header = %+v", got.Plan)
	}
	if len(got.Plan.Actions) != 1 {
		t.Fatalf("actions = %d", len(got.Plan.Actions))
	}
	a := got.Plan.Actions[0]
	if math.Abs(a.SoC-0.527) > 1e-9 {
		t.Fatalf("soc = %v, want 0.527", a.SoC)
	}
	if a.BatteryW != -577 {
		t.Fatalf("battery_w = %v, want -577", a.BatteryW)
	}
	if a.StorageEnergyWh["pixii"] != 10540 {
		t.Fatalf("storage_energy_wh = %#v", a.StorageEnergyWh)
	}
}

func TestPlanJSONSoCIsNotOmittedAtZero(t *testing.T) {
	body, err := json.Marshal(Action{SoC: 0, BatteryW: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["soc_pct"]; ok {
		t.Fatalf("Action JSON still emits soc_pct: %s", body)
	}
	v, ok := raw["soc"]
	if !ok {
		t.Fatalf("soc omitted from %s", body)
	}
	if v != 0.0 {
		t.Fatalf("soc = %v (%T), want 0", v, v)
	}
}
