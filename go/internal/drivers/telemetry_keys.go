package drivers

import "encoding/json"

// Driver emit tables, the Nova payload and @srcful/data-models grew three
// different spellings for the same measurement. nova.DerTelemetry unmarshals
// the driver's emit blob directly, so a key it does not recognise is dropped
// without an error and the value never leaves the gateway.
//
// That is not hypothetical. Catalog drivers emit import_wh, export_wh,
// charge_wh, discharge_wh, lifetime_wh and hz, while DerTelemetry reads
// total_import_wh, total_export_wh, total_charge_wh, total_discharge_wh,
// total_generation_wh and freq_hz. FTW's own zap.lua works around it by
// emitting both names for every such field.
//
// Normalising here fixes that for every driver at once, and lets
// srcfl/device-drivers move to the canonical @srcful/data-models spellings
// without a second round of host changes.
//
// The target name is the one nova.DerTelemetry reads. An alias never
// overwrites a key the driver already set under the target name.
var telemetryKeyAliases = map[string]string{
	// Canonical @srcful/data-models spellings.
	"W":                   "w",
	"Hz":                  "freq_hz",
	"SoC_nom_fract":       "soc",
	"temperature_C":       "temp_c",
	"total_import_Wh":     "total_import_wh",
	"total_export_Wh":     "total_export_wh",
	"total_charge_Wh":     "total_charge_wh",
	"total_discharge_Wh":  "total_discharge_wh",
	"total_generation_Wh": "total_generation_wh",
	"L1_V":                "l1_v",
	"L2_V":                "l2_v",
	"L3_V":                "l3_v",
	"L1_A":                "l1_a",
	"L2_A":                "l2_a",
	"L3_A":                "l3_a",
	"L1_W":                "l1_w",
	"L2_W":                "l2_w",
	"L3_W":                "l3_w",

	// Rated AC power reaches DerTelemetry as rated_power_w and arrives under
	// three other spellings: rated_W is the canonical @srcful/data-models
	// name, rated_power_W is what zap.lua emits, and rated_w is what most of
	// the catalog still uses. None of them was mapped, so a device's rated
	// power has never left the gateway.
	"rated_W":       "rated_power_w",
	"rated_power_W": "rated_power_w",
	"rated_w":       "rated_power_w",

	// Shorter spellings the catalog grew, which DerTelemetry never read.
	"hz":           "freq_hz",
	"import_wh":    "total_import_wh",
	"export_wh":    "total_export_wh",
	"charge_wh":    "total_charge_wh",
	"discharge_wh": "total_discharge_wh",
	"lifetime_wh":  "total_generation_wh",
}

// normalizeTelemetryKeys rewrites known aliases onto the names the Nova
// payload reads. The original keys are kept: the UI and API surface a
// reading's data verbatim, and removing a key a driver emitted would change
// what an operator sees.
//
// On any parse problem the input is returned untouched; emitTelemetry has
// already validated the envelope by this point and a normalisation failure
// must never lose a reading.
func normalizeTelemetryKeys(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}

	changed := false
	for alias, target := range telemetryKeyAliases {
		value, ok := fields[alias]
		if !ok {
			continue
		}
		if _, taken := fields[target]; taken {
			continue // the driver set the target name itself; it wins
		}
		fields[target] = value
		changed = true
	}
	if !changed {
		return raw
	}

	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}
