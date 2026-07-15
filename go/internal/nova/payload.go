package nova

// DerTelemetry is FTW's native clean DER telemetry payload.
// One instance per (device, DER) per tick. Every W value uses the site
// sign convention from docs/site-convention.md:
//
//   - Meter:   +W = import (grid→site),  −W = export
//   - PV:      ≤0 always (generation reduces import)
//   - Battery: +W = charging (a load),   −W = discharging (a source)
//   - EV:      +W when charging
//   - V2X:     +W = vehicle charging, -W = vehicle discharging
//
// Field naming is snake_case; units are in the name (freq_hz, temp_c,
// total_import_wh). Optional scalars are pointers so "zero" and "not
// reported" are distinguishable on the wire via `omitempty`.
//
// This is the source-of-truth schema. The adapter in adapter.go
// translates it to Nova's current (legacy) wire format; both shapes
// describe the same physical reality.
type DerTelemetry struct {
	// Envelope — always present.
	Type        string `json:"type"` // "meter" | "pv" | "battery" | "ev" | "v2x_charger"
	Make        string `json:"make,omitempty"`
	Model       string `json:"model,omitempty"`
	Serial      string `json:"serial,omitempty"`
	HardwareID  string `json:"hardware_id,omitempty"` // the FTW device_id
	TimestampMs int64  `json:"ts_ms"`                 // Unix millis

	// Core instantaneous power, site-signed.
	W float64 `json:"w"`

	// Temperature — inverter / heatsink / module. Drivers pick their most
	// representative sensor; if more are needed, use emit_metric.
	TempC *float64 `json:"temp_c,omitempty"`

	// Meter
	L1W           *float64 `json:"l1_w,omitempty"`
	L2W           *float64 `json:"l2_w,omitempty"`
	L3W           *float64 `json:"l3_w,omitempty"`
	L1V           *float64 `json:"l1_v,omitempty"`
	L2V           *float64 `json:"l2_v,omitempty"`
	L3V           *float64 `json:"l3_v,omitempty"`
	L1A           *float64 `json:"l1_a,omitempty"`
	L2A           *float64 `json:"l2_a,omitempty"`
	L3A           *float64 `json:"l3_a,omitempty"`
	FreqHz        *float64 `json:"freq_hz,omitempty"`
	TotalImportWh *float64 `json:"total_import_wh,omitempty"`
	TotalExportWh *float64 `json:"total_export_wh,omitempty"`

	// Battery
	SoC              *float64 `json:"soc,omitempty"` // 0..1 fraction of nominal
	CapacityWh       *float64 `json:"capacity_wh,omitempty"`
	DCV              *float64 `json:"dc_v,omitempty"`
	DCA              *float64 `json:"dc_a,omitempty"`
	DCW              *float64 `json:"dc_w,omitempty"`
	TotalChargeWh    *float64 `json:"total_charge_wh,omitempty"`
	TotalDischargeWh *float64 `json:"total_discharge_wh,omitempty"`

	// PV
	MPPT1V            *float64 `json:"mppt1_v,omitempty"`
	MPPT1A            *float64 `json:"mppt1_a,omitempty"`
	MPPT2V            *float64 `json:"mppt2_v,omitempty"`
	MPPT2A            *float64 `json:"mppt2_a,omitempty"`
	TotalGenerationWh *float64 `json:"total_generation_wh,omitempty"`

	// EV
	Connected  *bool    `json:"connected,omitempty"`
	Charging   *bool    `json:"charging,omitempty"`
	Phases     *int     `json:"phases,omitempty"`
	MaxA       *float64 `json:"max_a,omitempty"`
	SessionWh  *float64 `json:"session_wh,omitempty"`
	VehicleSoC *float64 `json:"vehicle_soc,omitempty"` // 0..1 fraction

	// V2X charger
	SessionChargeWh    *float64 `json:"session_charge_wh,omitempty"`
	SessionDischargeWh *float64 `json:"session_discharge_wh,omitempty"`
	ChargePowerMinW    *float64 `json:"charge_power_min_w,omitempty"`
	ChargePowerMaxW    *float64 `json:"charge_power_max_w,omitempty"`
	DischargePowerMinW *float64 `json:"discharge_power_min_w,omitempty"`
	DischargePowerMaxW *float64 `json:"discharge_power_max_w,omitempty"`
	EVMaxEnergyReqWh   *float64 `json:"ev_max_energy_req_wh,omitempty"`
	EVMinEnergyReqWh   *float64 `json:"ev_min_energy_req_wh,omitempty"`
	RatedPowerW        *float64 `json:"rated_power_w,omitempty"`
	Status             *string  `json:"status,omitempty"`
	Protocol           *string  `json:"protocol,omitempty"`
	ControlMode        *string  `json:"control_mode,omitempty"`
}

// DerKind is the clean vocabulary. Matches telemetry.DerType.String().
const (
	KindMeter   = "meter"
	KindPV      = "pv"
	KindBattery = "battery"
	KindEV      = "ev"
	KindV2X     = "v2x_charger"
)
