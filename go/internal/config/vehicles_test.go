package config

import "testing"

// Vehicle profiles are applied by identity match at transaction start, so
// validation must guarantee the match can never be ambiguous, and lookup
// must tolerate the case/whitespace variation of RFID tags, MACs and eMAIDs.
func TestVehicleValidationAndLookup(t *testing.T) {
	c := &Config{Vehicles: []Vehicle{
		{ID: "leaf", Name: "Nissan Leaf", CapacityWh: 40000,
			Identifiers: []string{"04A2B3C4", "aa:bb:cc:dd:ee:ff"}},
		{ID: "model3", CapacityWh: 75000, Identifiers: []string{"DEADBEEF"},
			SurplusOnly: true, TargetSoC: 0.80},
	}}
	if err := c.validateVehicles(); err != nil {
		t.Fatalf("valid vehicles rejected: %v", err)
	}
	if v := c.VehicleByIdentifier("  aa:BB:cc:DD:ee:FF "); v == nil || v.ID != "leaf" {
		t.Fatalf("MAC should match leaf, got %+v", v)
	}
	if v := c.VehicleByIdentifier("deadbeef"); v == nil || v.ID != "model3" {
		t.Fatalf("tag should match model3, got %+v", v)
	}
	if v := c.VehicleByIdentifier("unknown"); v != nil {
		t.Fatalf("unknown identifier matched %+v", v)
	}
	if v := c.VehicleByIdentifier("  "); v != nil {
		t.Fatalf("blank identifier matched %+v", v)
	}

	dup := &Config{Vehicles: []Vehicle{
		{ID: "a", Identifiers: []string{"TAG1"}},
		{ID: "b", Identifiers: []string{" tag1 "}},
	}}
	if err := dup.validateVehicles(); err == nil {
		t.Fatal("identifier claimed by two vehicles should fail validation")
	}
	if err := (&Config{Vehicles: []Vehicle{{ID: "a"}, {ID: "a"}}}).validateVehicles(); err == nil {
		t.Fatal("duplicate vehicle id should fail validation")
	}
	if err := (&Config{Vehicles: []Vehicle{{ID: "x", TargetSoC: 1.5}}}).validateVehicles(); err == nil {
		t.Fatal("target_soc > 1 should fail validation")
	}
	if err := (&Config{Vehicles: []Vehicle{{ID: "x", Identifiers: []string{" "}}}}).validateVehicles(); err == nil {
		t.Fatal("blank identifier should fail validation")
	}
}
