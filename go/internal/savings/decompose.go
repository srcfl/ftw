package savings

// decomposeFlows splits one slot's measured kWh into the seven channels
// the UI uses to explain "where did each kWh come from / go to". This is
// presentation-only — none of these numbers feed back into ActualOre or
// NoBatteryOre.
//
// The split is computed on aggregate energy totals for the slot. If a
// slot's measurements aren't perfectly self-consistent (driver lag, grid
// meter and battery meter sampled at slightly different times) the
// decomposition's identities hold to within the same noise. We don't
// attempt sub-slot phase-tracking — the cost numbers don't need it, and
// the pedagogical picture is the same when averaged over a few slots.
//
// Algorithm:
//
//  1. Self-consumption first: min(pv, load).
//  2. PV surplus (if any) goes to battery first, then to grid.
//  3. Load deficit (if any) is filled by battery first, then by grid.
//  4. If the battery charged more than PV could provide, the rest came
//     from the grid (arbitrage charging at cheap hours).
//  5. If the battery discharged more than load needed, the rest went to
//     the grid (arbitrage discharging at peak hours).
//
// Edge case: a slot can have both bat_charged_kwh and bat_discharged_kwh
// > 0 if the battery flipped direction within the slot. The two flows are
// then attributed independently — charging gets PV/grid sources, dis-
// charging routes to home/grid sinks — and the slot will report both
// PVToBat and BatToHome (or similar) as nonzero, which is honest about
// what physically happened.
func decomposeFlows(s slotIntegrals) FlowBreakdown {
	pv := s.PVKWh
	load := s.LoadKWh
	charged := s.BatChargedKWh
	discharged := s.BatDischargedKWh

	self := minPos(pv, load)
	pvSurplus := pv - self
	loadDeficit := load - self

	pvToBat := minPos(pvSurplus, charged)
	directExport := pvSurplus - pvToBat
	gridToBat := charged - pvToBat
	if gridToBat < 0 {
		gridToBat = 0
	}

	batToHome := minPos(loadDeficit, discharged)
	gridToHome := loadDeficit - batToHome
	if gridToHome < 0 {
		gridToHome = 0
	}
	batToGrid := discharged - batToHome
	if batToGrid < 0 {
		batToGrid = 0
	}

	return FlowBreakdown{
		SelfConsumptionKWh: self,
		DirectExportKWh:    directExport,
		PVToBatKWh:         pvToBat,
		BatToHomeKWh:       batToHome,
		BatToGridKWh:       batToGrid,
		GridToHomeKWh:      gridToHome,
		GridToBatKWh:       gridToBat,
	}
}

// minPos returns the smaller of a, b clamped at 0. Avoids negative leaks
// when one input is sub-zero from rounding.
func minPos(a, b float64) float64 {
	m := a
	if b < m {
		m = b
	}
	if m < 0 {
		return 0
	}
	return m
}
