package v2x

import (
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

func TestEvaluateDisabledPolicyCollapsesToZero(t *testing.T) {
	env := Evaluate(nil, Snapshot{Driver: "dc2"})
	if env.MinPowerW != 0 || env.MaxPowerW != 0 {
		t.Fatalf("disabled envelope = [%v,%v], want [0,0]", env.MinPowerW, env.MaxPowerW)
	}
	if !hasReason(env, "policy_disabled") {
		t.Fatalf("missing policy_disabled reason: %+v", env.Reasons)
	}
}

func TestEvaluateRequiresConnectedVehicleAndSoC(t *testing.T) {
	p := basePolicy()
	disconnected := false
	soc := 0.7
	env := Evaluate(p, baseSnapshot(Snapshot{Connected: &disconnected, SoC: &soc}))
	if env.MinPowerW != 0 || env.MaxPowerW != 0 || !hasReason(env, "vehicle_disconnected") {
		t.Fatalf("disconnected envelope = %+v", env)
	}

	connected := true
	snap := baseSnapshot(Snapshot{Connected: &connected})
	snap.SoC = nil
	env = Evaluate(p, snap)
	if env.MinPowerW != 0 || env.MaxPowerW != 0 || !hasReason(env, "soc_missing") {
		t.Fatalf("missing SoC envelope = %+v", env)
	}
}

func TestEvaluateReserveBlocksDischargeButAllowsCharge(t *testing.T) {
	p := basePolicy()
	soc := 0.2
	env := Evaluate(p, baseSnapshot(Snapshot{SoC: &soc}))
	if env.MinPowerW != 0 {
		t.Fatalf("discharge below reserve: min_power_w = %v, want 0", env.MinPowerW)
	}
	if env.MaxPowerW != 3500 {
		t.Fatalf("charge power = %v, want 3500", env.MaxPowerW)
	}
	if !hasReason(env, "reserve_floor") {
		t.Fatalf("missing reserve_floor reason: %+v", env.Reasons)
	}
}

func TestEvaluateDepartureTargetBlocksDischargeWhenRechargeWindowIsTight(t *testing.T) {
	p := basePolicy()
	p.DepartureTargetSoCPct = 80
	p.DepartureTime = "08:00"
	p.VehicleCapacityWh = 100_000
	p.MaxChargeW = 7_000
	soc := 0.72
	now := time.Date(2026, 6, 11, 7, 30, 0, 0, time.Local)

	env := Evaluate(p, baseSnapshot(Snapshot{SoC: &soc, Now: now}))
	if env.MinPowerW != 0 {
		t.Fatalf("departure-constrained min_power_w = %v, want 0", env.MinPowerW)
	}
	if !hasReason(env, "departure_target_floor") {
		t.Fatalf("missing departure_target_floor reason: %+v", env.Reasons)
	}
}

func TestEvaluateDepartureFloorIgnoresGridRechargeWhenGridChargingForbidden(t *testing.T) {
	// Grid charging is forbidden and the site is importing (no PV surplus),
	// so no guaranteed recharge exists before departure. The departure floor
	// must therefore stay at the full target and block discharge — even at a
	// SoC the old full-charge-rate credit would have released.
	p := basePolicy()
	p.ExportAllowed = true
	p.GridChargingAllowed = false
	p.DepartureTargetSoCPct = 80
	p.DepartureTime = "08:00"
	p.VehicleCapacityWh = 100_000
	p.MaxChargeW = 7_000
	soc := 0.70 // above old requiredNow (0.66), below new requiredNow (0.80)
	now := time.Date(2026, 6, 11, 6, 0, 0, 0, time.Local)
	importing := 1500.0 // positive = importing, no surplus

	env := Evaluate(p, baseSnapshot(Snapshot{SoC: &soc, Now: now, GridW: &importing}))
	if env.MinPowerW != 0 {
		t.Fatalf("discharge must be blocked without guaranteed recharge: min_power_w = %v, want 0", env.MinPowerW)
	}
	if !hasReason(env, "departure_target_floor") {
		t.Fatalf("missing departure_target_floor reason: %+v", env.Reasons)
	}
}

func TestEvaluateExportAndGridChargingLimitsUseCurrentGridPower(t *testing.T) {
	p := basePolicy()
	p.ExportAllowed = false
	p.GridChargingAllowed = false
	soc := 0.8

	importing := 900.0
	env := Evaluate(p, baseSnapshot(Snapshot{SoC: &soc, GridW: &importing}))
	if env.MinPowerW != -900 {
		t.Fatalf("discharge should cap to current import: min_power_w = %v, want -900", env.MinPowerW)
	}
	if env.MaxPowerW != 0 {
		t.Fatalf("charge while importing should be blocked: max_power_w = %v, want 0", env.MaxPowerW)
	}
	if !hasReason(env, "export_limited_to_import") || !hasReason(env, "grid_charging_blocked") {
		t.Fatalf("missing grid/export reasons: %+v", env.Reasons)
	}

	exporting := -1200.0
	env = Evaluate(p, baseSnapshot(Snapshot{SoC: &soc, GridW: &exporting}))
	if env.MinPowerW != 0 {
		t.Fatalf("discharge while exporting should be blocked: min_power_w = %v, want 0", env.MinPowerW)
	}
	if env.MaxPowerW != 1200 {
		t.Fatalf("charge should cap to current surplus: max_power_w = %v, want 1200", env.MaxPowerW)
	}
	if !hasReason(env, "export_blocked") || !hasReason(env, "charge_limited_to_surplus") {
		t.Fatalf("missing surplus reasons: %+v", env.Reasons)
	}
}

func basePolicy() *config.V2XPolicy {
	return &config.V2XPolicy{
		Enabled:             true,
		MinReserveSoCPct:    20,
		MaxChargeW:          3500,
		MaxDischargeW:       3200,
		ExportAllowed:       true,
		GridChargingAllowed: true,
	}
}

func baseSnapshot(overrides Snapshot) Snapshot {
	connected := true
	soc := 0.6
	gridW := 2000.0
	out := Snapshot{
		Driver:             "dc2",
		Online:             true,
		Connected:          &connected,
		SoC:                &soc,
		CapacityWh:         80_000,
		ChargePowerMaxW:    7400,
		DischargePowerMaxW: 7400,
		RatedPowerW:        11_000,
		GridW:              &gridW,
		Now:                time.Date(2026, 6, 11, 12, 0, 0, 0, time.Local),
	}
	if overrides.Driver != "" {
		out.Driver = overrides.Driver
	}
	if overrides.Connected != nil {
		out.Connected = overrides.Connected
	}
	if overrides.SoC != nil {
		out.SoC = overrides.SoC
	}
	if overrides.CapacityWh != 0 {
		out.CapacityWh = overrides.CapacityWh
	}
	if overrides.ChargePowerMaxW != 0 {
		out.ChargePowerMaxW = overrides.ChargePowerMaxW
	}
	if overrides.DischargePowerMaxW != 0 {
		out.DischargePowerMaxW = overrides.DischargePowerMaxW
	}
	if overrides.RatedPowerW != 0 {
		out.RatedPowerW = overrides.RatedPowerW
	}
	if overrides.GridW != nil {
		out.GridW = overrides.GridW
	}
	if !overrides.Now.IsZero() {
		out.Now = overrides.Now
	}
	return out
}

func hasReason(env Envelope, reason string) bool {
	for _, got := range env.Reasons {
		if got == reason {
			return true
		}
	}
	return false
}
