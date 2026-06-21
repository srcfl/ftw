package nova

import (
	"encoding/json"
	"time"
)

// SchemaMode selects the wire format produced by Encode.
type SchemaMode string

const (
	// SchemaLegacy translates to the current Nova wire format —
	// mixed-case fields, opposite battery sign, solar/ev_port
	// vocabulary. This is what Nova (and hundreds of deployed ZAP
	// gateways) speak today.
	SchemaLegacy SchemaMode = "legacy"
	// SchemaUnified emits forty-two-watts' native clean payload
	// as-is. Flip to this once Nova's unified schema ships.
	SchemaUnified SchemaMode = "unified"
)

// Encode serializes a DerTelemetry to the configured wire format.
// On SchemaLegacy, the return is the JSON that Nova's topic-router
// and metrics-bridge expect today; on SchemaUnified, the return is
// the DerTelemetry shape verbatim.
//
// NOTE: legacy translation is *only* here. It does not leak into
// payload.go, the publisher, the telemetry store, or anywhere else.
// That isolation is the point — the adapter is the deletable bridge.
func Encode(t *DerTelemetry, mode SchemaMode) ([]byte, error) {
	if mode == SchemaUnified {
		return json.Marshal(t)
	}
	return json.Marshal(toLegacy(t))
}

// legacyPayload mirrors the current Nova wire shape. PascalCase-with-
// underscores is Nova's chosen naming (not ours) and will go away
// when the unified schema lands.
type legacyPayload struct {
	Type       string `json:"type"`
	Make       string `json:"make,omitempty"`
	Timestamp  string `json:"timestamp"`
	ReadTimeMs int64  `json:"read_time_ms"`

	// Common / meter
	W    *float64 `json:"W,omitempty"`
	Hz   *float64 `json:"Hz,omitempty"`
	L1V  *float64 `json:"L1_V,omitempty"`
	L2V  *float64 `json:"L2_V,omitempty"`
	L3V  *float64 `json:"L3_V,omitempty"`
	L1A  *float64 `json:"L1_A,omitempty"`
	L2A  *float64 `json:"L2_A,omitempty"`
	L3A  *float64 `json:"L3_A,omitempty"`
	TIWh *float64 `json:"total_import_Wh,omitempty"`
	TEWh *float64 `json:"total_export_Wh,omitempty"`

	// Battery
	A              *float64 `json:"A,omitempty"`
	V              *float64 `json:"V,omitempty"`
	DCW            *float64 `json:"dc_W,omitempty"`
	DCV            *float64 `json:"dc_V,omitempty"`
	DCA            *float64 `json:"dc_A,omitempty"`
	SoCFract       *float64 `json:"SoC_nom_fract,omitempty"`
	TotalChargeWh  *float64 `json:"total_charge_Wh,omitempty"`
	TotalDischarWh *float64 `json:"total_discharge_Wh,omitempty"`
	HeatsinkC      *float64 `json:"heatsink_C,omitempty"`
	CapacityWh     *float64 `json:"capacity_Wh,omitempty"`

	// PV (shares HeatsinkC above)
	RatedPowerW       *float64 `json:"rated_power_W,omitempty"`
	MPPT1V            *float64 `json:"mppt1_V,omitempty"`
	MPPT1A            *float64 `json:"mppt1_A,omitempty"`
	MPPT2V            *float64 `json:"mppt2_V,omitempty"`
	MPPT2A            *float64 `json:"mppt2_A,omitempty"`
	TotalGenerationWh *float64 `json:"total_generation_Wh,omitempty"`

	// EV (ev_port in legacy vocabulary)
	PlugConnected    *bool     `json:"plug_connected,omitempty"`
	Status           *string   `json:"status,omitempty"`
	Protocol         *string   `json:"protocol,omitempty"`
	ControlMode      *string   `json:"control_mode,omitempty"`
	VehicleSoCFract  *float64  `json:"vehicle_soc_fract,omitempty"`
	EVMaxEnergyReqWh *float64  `json:"ev_max_energy_req_Wh,omitempty"`
	EVMinEnergyReqWh *float64  `json:"ev_min_energy_req_Wh,omitempty"`
	SessionChargeWh  *float64  `json:"session_charge_Wh,omitempty"`
	SessionDischarWh *float64  `json:"session_discharge_Wh,omitempty"`
	LowerLimitW      []float64 `json:"lower_limit_W,omitempty"`
	UpperLimitW      []float64 `json:"upper_limit_W,omitempty"`
}

// toLegacy performs the translation. It is a pure function of the
// input — callers use Encode, tests call this directly.
//
// Translations:
//   - type "pv" → "solar", "ev" → "ev_port"
//   - battery W sign: site convention (+ charge) → Nova convention (− charge)
//   - field renames: w→W, freq_hz→Hz, l1_v→L1_V, soc→SoC_nom_fract,
//     temp_c→heatsink_C, connected→plug_connected, vehicle_soc→
//     vehicle_soc_fract, session_wh→session_charge_Wh, dc_v→V, dc_a→A
//   - derives status string for EV from connected/charging booleans.
func toLegacy(t *DerTelemetry) *legacyPayload {
	if t == nil {
		return nil
	}
	out := &legacyPayload{
		Type:       legacyType(t.Type),
		Make:       t.Make,
		Timestamp:  time.UnixMilli(t.TimestampMs).UTC().Format(time.RFC3339Nano),
		ReadTimeMs: t.TimestampMs,
	}
	w := t.W
	// Battery sign flip. f42w: +charge (load). Nova: −charge.
	if t.Type == KindBattery {
		w = -w
	}
	out.W = &w

	// Common scalars that pass through.
	out.Hz = t.FreqHz
	out.L1V, out.L2V, out.L3V = t.L1V, t.L2V, t.L3V
	out.L1A, out.L2A, out.L3A = t.L1A, t.L2A, t.L3A
	out.TIWh = t.TotalImportWh
	out.TEWh = t.TotalExportWh

	// Temp routes to heatsink_C for both PV and battery in legacy shape.
	out.HeatsinkC = t.TempC

	// Battery specifics.
	if t.Type == KindBattery {
		out.A = t.DCA
		out.V = t.DCV
		out.SoCFract = t.SoC
		out.CapacityWh = t.CapacityWh
		out.TotalChargeWh = t.TotalChargeWh
		out.TotalDischarWh = t.TotalDischargeWh
	}

	// PV specifics.
	if t.Type == KindPV {
		out.MPPT1V, out.MPPT1A = t.MPPT1V, t.MPPT1A
		out.MPPT2V, out.MPPT2A = t.MPPT2V, t.MPPT2A
		out.TotalGenerationWh = t.TotalGenerationWh
	}

	// EV specifics.
	if t.Type == KindEV {
		out.PlugConnected = t.Connected
		out.VehicleSoCFract = t.VehicleSoC
		out.SessionChargeWh = t.SessionWh
		if s := evStatus(t); s != "" {
			out.Status = &s
		}
	}
	if t.Type == KindV2X {
		out.PlugConnected = t.Connected
		out.VehicleSoCFract = t.VehicleSoC
		out.DCW = t.DCW
		out.DCV = t.DCV
		out.DCA = t.DCA
		out.CapacityWh = t.CapacityWh
		out.TotalChargeWh = t.TotalChargeWh
		out.TotalDischarWh = t.TotalDischargeWh
		out.SessionChargeWh = t.SessionChargeWh
		out.SessionDischarWh = t.SessionDischargeWh
		out.EVMaxEnergyReqWh = t.EVMaxEnergyReqWh
		out.EVMinEnergyReqWh = t.EVMinEnergyReqWh
		if t.ChargePowerMinW != nil || t.ChargePowerMaxW != nil {
			out.UpperLimitW = []float64{valueOrZero(t.ChargePowerMinW), 0, valueOrZero(t.ChargePowerMaxW)}
		}
		if t.DischargePowerMinW != nil || t.DischargePowerMaxW != nil {
			out.LowerLimitW = []float64{-valueOrZero(t.DischargePowerMaxW), 0, -valueOrZero(t.DischargePowerMinW)}
		}
		out.RatedPowerW = t.RatedPowerW
		out.Status = t.Status
		out.Protocol = t.Protocol
		out.ControlMode = t.ControlMode
	}
	return out
}

func valueOrZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// legacyType maps the clean DER vocabulary to Nova's current wire vocabulary.
func legacyType(kind string) string {
	switch kind {
	case KindPV:
		return "solar"
	case KindEV:
		return "ev_port"
	case KindV2X:
		return "v2x_charger"
	default:
		return kind
	}
}

// evStatus derives Nova's `status` string from the clean connected/charging
// booleans. Kept intentionally small — the existing gateway-simulator uses
// a very coarse state machine and we mirror that.
func evStatus(t *DerTelemetry) string {
	if t.Charging != nil && *t.Charging {
		return "charging"
	}
	if t.Connected != nil && *t.Connected {
		return "connected"
	}
	if t.Connected != nil && !*t.Connected {
		return "disconnected"
	}
	return ""
}
