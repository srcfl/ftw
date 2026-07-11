package drivers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// The Ferroamp driver must surface its per-direction capability on the
// battery emit so the dispatcher can reallocate around a floored pack
// (the eso_*_capable counts already drive its own dispatch scaling). Below
// DISCHARGE_FLOOR_SOC (default 0.15) the single ESO is not discharge-capable
// → discharge_capable=false, while it can still charge.
func TestFerroampEmitsDischargeBlockedBelowFloor(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	ehub := `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0}}`
	// soc 10 % → 0.10 < 0.15 floor → cannot discharge, can still charge.
	eso := `{"pbat":{"val":0},"soc":{"val":10},"ubat":{"val":48},"ibat":{"val":0}}`
	mqtt.Push("extapi/data/ehub", ehub)
	mqtt.Push("extapi/data/eso", eso)

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	bat := tel.Get("ferroamp", telemetry.DerBattery)
	if bat == nil {
		t.Fatal("expected battery reading")
	}
	var c struct {
		DischargeCapable *bool `json:"discharge_capable"`
		ChargeCapable    *bool `json:"charge_capable"`
	}
	if err := json.Unmarshal(bat.Data, &c); err != nil {
		t.Fatalf("battery Data not JSON: %v (data=%s)", err, bat.Data)
	}
	if c.DischargeCapable == nil || *c.DischargeCapable {
		t.Errorf("discharge_capable = %v, want false (soc below floor)", c.DischargeCapable)
	}
	if c.ChargeCapable == nil || !*c.ChargeCapable {
		t.Errorf("charge_capable = %v, want true", c.ChargeCapable)
	}
}

// Above the floor the ESO is discharge-capable, so the field is emitted true.
func TestFerroampEmitsDischargeCapableAboveFloor(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	ehub := `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0}}`
	eso := `{"pbat":{"val":0},"soc":{"val":50},"ubat":{"val":48},"ibat":{"val":0}}`
	mqtt.Push("extapi/data/ehub", ehub)
	mqtt.Push("extapi/data/eso", eso)

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	bat := tel.Get("ferroamp", telemetry.DerBattery)
	if bat == nil {
		t.Fatal("expected battery reading")
	}
	var c struct {
		DischargeCapable *bool `json:"discharge_capable"`
	}
	if err := json.Unmarshal(bat.Data, &c); err != nil {
		t.Fatalf("battery Data not JSON: %v", err)
	}
	if c.DischargeCapable == nil || !*c.DischargeCapable {
		t.Errorf("discharge_capable = %v, want true (soc above floor)", c.DischargeCapable)
	}
}
