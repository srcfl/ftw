// Package units is the source of truth for physical quantities in FTW core.
//
// Core stores and computes:
//
//	Power       watts (W)
//	Energy      watt-hours (Wh)
//	Irradiance  watts per square metre (W/m²)
//	Fraction    0..1 (SoC, efficiency, and any other ratio)
//
// Kilo- prefixes and 0–100 percents exist only at a door:
//
//	UI display, Home Assistant %, appproto permille, forecast.solar kWp URL,
//	optimizer protocol v1 soc_pct JSON, and legacy YAML on load.
//
// Physics gates (nameplate ceiling, SoC in [0,1]) still apply. They are not
// a substitute for storing the right unit.
package units

import (
	"math"
	"strings"
)

// STCIrradianceWm2 is standard-test-condition irradiance. PV power at STC
// equals the array's rated watts; at other irradiance
//
//	P = rated_w × (G / STCIrradianceWm2)
const STCIrradianceWm2 = 1000.0

// WattsFromKWp converts a vendor/UI kilowatt-peak figure to watts.
// Use only at a door (forecast.solar URL, display). Core stores watts.
func WattsFromKWp(kwp float64) float64 {
	return kwp * 1000.0
}

// KWpFromWatts converts rated watts to kilowatt-peak for a vendor that
// speaks kWp (forecast.solar). Core must not persist the result.
func KWpFromWatts(w float64) float64 {
	if w <= 0 {
		return 0
	}
	return w / 1000.0
}

// CanonicalPowerEnergy is the emit_metric door for vendor kilo-units.
// kW/kWh become W/Wh. Other units pass through, including W and Wh
// already converted in a Lua driver. Unit matching is case-insensitive so
// a driver that emits "kw" cannot store kilowatts as watts. Non-finite
// values become 0 rather than propagating NaN into history or HA.
func CanonicalPowerEnergy(value float64, unit string) (float64, string) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		value = 0
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "kw":
		return value * 1000.0, "W"
	case "kwh":
		return value * 1000.0, "Wh"
	default:
		return value, unit
	}
}

// PVFromIrradiance is the STC scale: rated watts at 1000 W/m².
func PVFromIrradiance(ratedW, irradianceWm2 float64) float64 {
	if ratedW <= 0 || irradianceWm2 <= 0 {
		return 0
	}
	return ratedW * (irradianceWm2 / STCIrradianceWm2)
}

// FractionFromLegacyPercent maps a value that may still be 0–100 into 0–1.
// Values already in (0, 1] pass through. 0 stays 0. Call only when loading
// old YAML/JSON; new code writes 0–1.
//
// Values in (1, 2) are treated as 0–1 overflow (BMS 102%, a 1.5 typo on a
// fraction field), not as 1.02%/1.5%. Real percents start at 2. The result
// is not clamped to [0, 1] after /100: 150 stays 1.5 so ValidFraction can
// still reject it rather than silently storing 100%.
func FractionFromLegacyPercent(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 && v < 2 {
		return 1
	}
	if v > 1 {
		return v / 100.0
	}
	return v
}

// DecodeJSONFraction is the HTTP/JSON inbound door. Canonical (already 0–1)
// wins when set; otherwise legacyPercent (0–100 or already 0–1) is folded.
func DecodeJSONFraction(canonical, legacyPercent float64) float64 {
	v := canonical
	if v == 0 && legacyPercent != 0 {
		v = legacyPercent
	}
	return FractionFromLegacyPercent(v)
}

// DefaultPluginSoC is the conservative EV plug-in assumption when the
// operator has not set plugin_soc. Chargers like Easee do not report BMS.
const DefaultPluginSoC = 0.20

// PercentFromFraction is the UI/HA door. Core must not store the result.
// Non-finite and out-of-range fractions fold onto [0, 100] so a NaN SoC
// cannot become "NaN%" on a chart or in a support dump.
func PercentFromFraction(f float64) float64 {
	return ClampFraction(f) * 100.0
}

// PermilleFromFraction is the appproto door (field battery_soc).
func PermilleFromFraction(f float64) int64 {
	return int64(math.Round(ClampFraction(f) * 1000.0))
}

// FractionFromPermille is the appproto inbound door.
func FractionFromPermille(p int64) float64 {
	return ClampFraction(float64(p) / 1000.0)
}

// ValidFraction reports whether f is a finite value in [0, 1].
func ValidFraction(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 1
}

// ClampFraction folds a finite value onto [0, 1]. Non-finite → 0.
func ClampFraction(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// RatedWattsFromLegacyKWp is the YAML door for weather.pv_arrays[].kwp.
//
// Historical configs stored kilowatt-peak in `kwp`. Operators also pasted
// weather.pv_rated_w (watts) into that field, so 18960 meant 18.96 kW.
// Values ≥ 1000 are treated as watts already; smaller values are kWp.
//
// New configs set rated_w (watts) and leave kwp unset. Do not call this
// from forecast/planner math — only from config load.
func RatedWattsFromLegacyKWp(kwp float64) float64 {
	if math.IsNaN(kwp) || math.IsInf(kwp, 0) || kwp <= 0 {
		return 0
	}
	if kwp >= 1000 {
		return kwp
	}
	return WattsFromKWp(kwp)
}
