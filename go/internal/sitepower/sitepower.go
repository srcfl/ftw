// Package sitepower is the site-boundary power identity used by charging
// policy tests and (later) by planner/dispatch.
//
// Core stores watts in the site sign convention: positive into the site.
// The meter identity with every DER named separately is
//
//	grid = load + pv + battery + ev + v2x
//
// House leftover PV is max(0, −(load+pv)). A surplus-only EV may take
// leftover, never house-battery discharge, and never grid import that is
// not leftover. Battery soak of leftover is not the same as the battery
// buying from the grid: soak reduces what the EV may take; a battery
// charge above leftover is a grid buy and leaves leftover available to
// the EV (the #957 leftover-PV-beside-battery-grid-charge case).
package sitepower

import "math"

// GridChargeImportW is the live-meter deadband used to tell "the site is
// importing" from noise. Matches the existing 50–100 W bands in MPC/control.
const GridChargeImportW = 100.0

// GridW is the five-term site identity. PV is ≤0, EV charging is ≥0,
// battery charge is >0, battery discharge is <0.
func GridW(loadW, pvW, batteryW, evW, v2xW float64) float64 {
	return loadW + pvW + batteryW + evW + v2xW
}

// HouseLeftoverW is PV remaining after covering house load, in watts.
// Zero when the house is in deficit. PV must be site-signed (negative).
func HouseLeftoverW(loadW, pvW float64) float64 {
	if math.IsNaN(loadW) || math.IsInf(loadW, 0) || math.IsNaN(pvW) || math.IsInf(pvW, 0) {
		return 0
	}
	v := -(loadW + pvW)
	if v < 0 {
		return 0
	}
	return v
}

// SurplusAvailableForEVW is leftover PV the surplus-only EV may take this
// tick after the house battery's own soak.
//
//	leftover leftover, battery charging ≤ leftover → leftover − soak
//	battery charging > leftover → leftover (battery is buying grid)
//	battery idle or discharging → leftover (discharge into the car is a
//	separate no-battery-to-EV check)
func SurplusAvailableForEVW(loadW, pvW, batteryW float64) float64 {
	leftover := HouseLeftoverW(loadW, pvW)
	if leftover <= 0 {
		return 0
	}
	if math.IsNaN(batteryW) || math.IsInf(batteryW, 0) {
		return 0
	}
	if batteryW > leftover {
		return leftover
	}
	if batteryW > 0 {
		return leftover - batteryW
	}
	return leftover
}

// SurplusOnlyEVExceedsLeftover reports that evW cannot be explained as
// leftover PV. Use this, not "gridW > 50", to decide whether a surplus-only
// EV imported. Grid import during a battery grid-buy plus leftover EV is
// legal; grid import from soak+EV oversubscription is not.
func SurplusOnlyEVExceedsLeftover(loadW, pvW, batteryW, evW float64) bool {
	if math.IsNaN(evW) || math.IsInf(evW, 0) || evW <= 0 {
		return false
	}
	return evW > SurplusAvailableForEVW(loadW, pvW, batteryW)+GridChargeImportW
}

// BatteryFeedsEV reports conservation: some of a discharging house battery
// must have landed in the EV (or on the grid — callers that already forbid
// battery export can treat this as battery-to-EV).
func BatteryFeedsEV(loadW, pvW, batteryW, evW float64) bool {
	if batteryW >= -GridChargeImportW || evW <= 0 {
		return false
	}
	// House residual after PV: positive means the house still needs energy.
	houseNeed := loadW + pvW
	if houseNeed < 0 {
		houseNeed = 0
	}
	discharge := -batteryW
	return discharge > houseNeed+GridChargeImportW
}

// Finite reports a usable watt value.
func Finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
