package mpc

// Fixed vocabulary for why buildMPC left the planner off. The Plan view
// and /api/mpc/diagnose use the same strings so a support question
// ("why won't it plan?") does not require boot logs.
const (
	ReasonPlannerDisabled   = "planner-disabled"
	ReasonNoPriceProvider   = "no-price-provider"
	ReasonNoBatteryCapacity = "no-battery-capacity"
)

// UnavailableReason is the skip gate buildMPC already logs. Empty means
// the planner should have been constructed.
func UnavailableReason(plannerEnabled bool, priceProvider string, totalCapacityWh float64, supportsBatteryless bool) string {
	if !plannerEnabled {
		return ReasonPlannerDisabled
	}
	if priceProvider == "" || priceProvider == "none" {
		return ReasonNoPriceProvider
	}
	if totalCapacityWh < 0 || (totalCapacityWh == 0 && !supportsBatteryless) {
		return ReasonNoBatteryCapacity
	}
	return ""
}
