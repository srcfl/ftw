package loadpoint

// Site power identity (site-signed: PV generation is negative, battery
// charge and EV charge are positive, grid import is positive):
//
//	gridW = loadW + pvW + batteryW + evW
//
// Leftover PV after the house and house residual after PV are the two
// sides of the same number. Surplus-only EV and "battery may not feed
// EV" are policies on this identity, not extra meters.
const SitePowerEpsW = 50

// GridW is the meter flow implied by house, PV, battery and EV.
func GridW(loadW, pvW, batteryW, evW float64) float64 {
	return loadW + pvW + batteryW + evW
}

// PVLeftoverAfterHouseW is PV remaining after house load. Zero when the
// house consumes the whole array (or more).
func PVLeftoverAfterHouseW(loadW, pvW float64) float64 {
	leftover := -(loadW + pvW)
	if leftover < 0 {
		return 0
	}
	return leftover
}

// HouseResidualW is house demand still to be covered after PV. Zero when
// PV covers the house (or more). Battery discharge up to this amount can
// still be claimed as "house only"; anything beyond it feeds the EV or
// the grid.
func HouseResidualW(loadW, pvW float64) float64 {
	residual := loadW + pvW
	if residual < 0 {
		return 0
	}
	return residual
}

// SurplusOnlyExceedsHousePV reports whether evW would have to come from
// the grid or the home battery rather than leftover PV. Site import from
// a simultaneous home-battery grid-charge is not the car importing.
func SurplusOnlyExceedsHousePV(evW, loadW, pvW float64) bool {
	return evW > PVLeftoverAfterHouseW(loadW, pvW)+SitePowerEpsW
}

// BatteryDischargeFeedsEV reports whether a simultaneous battery
// discharge and EV charge would, by conservation, put battery energy
// into the car.
func BatteryDischargeFeedsEV(batteryW, evW, loadW, pvW float64) bool {
	if evW <= 0 || batteryW >= 0 {
		return false
	}
	return -batteryW > HouseResidualW(loadW, pvW)+SitePowerEpsW
}

// PlannedSurplusForEVW is the near-term 3Φ-gate quantity: leftover PV
// after house load, minus planned PV-soak battery charge. gridW is the
// grid flow attributed to house+battery (planned GridW minus EV).
// Grid-funded battery charge does not consume leftover the car can take.
func PlannedSurplusForEVW(loadW, pvW, batteryW, gridW float64) float64 {
	return -pvW - loadW - PlannedPVSoakW(batteryW, gridW)
}

// BatteryEnergyDeltaWh is the cell-side energy change for a site-signed
// AC battery power over dtH hours. Charge (powerW > 0) lands ηc of the
// AC energy in the cells. Discharge (powerW < 0) draws AC / ηd from the
// cells, so a 1000 W discharge at 0.95 efficiency removes ~1053 Wh/h.
func BatteryEnergyDeltaWh(powerW, dtH, chargeEff, dischargeEff float64) float64 {
	if powerW >= 0 {
		return powerW * dtH * chargeEff
	}
	return powerW * dtH / dischargeEff
}

// EffectivePVW is the site-signed PV used in grid replay after an
// optional curtailment cap. Inactive means the forecast stands.
// Active with pvLimitW = 0 is a true zero cap (no generation this slot).
func EffectivePVW(pvW, pvLimitW float64, curtailActive bool) float64 {
	if !curtailActive {
		return pvW
	}
	if pvLimitW < 0 {
		pvLimitW = 0
	}
	return -pvLimitW
}
