// Package gridcost contains the shared import/export pricing model used by
// planner decisions, plan reporting, and historical savings calculations.
package gridcost

// ExportPricing captures the runtime knobs that turn a slot's raw spot price
// into the ore/kWh the household earns, or pays, when exporting.
type ExportPricing struct {
	BonusOreKwh float64
	FeeOreKwh   float64
	FlatOreKwh  float64
	FloorOreKwh *float64
}

// ExportPriceOre returns the ore/kWh for exported energy. A flat export price
// wins when configured. Otherwise the model is spot + bonus - fee, optionally
// floored by FloorOreKwh. Negative values are intentional: pass-through export
// during negative spot can cost money unless the retailer explicitly floors it.
func ExportPriceOre(spotOreKwh float64, ep ExportPricing) float64 {
	if ep.FlatOreKwh > 0 {
		return ep.FlatOreKwh
	}
	v := spotOreKwh + ep.BonusOreKwh - ep.FeeOreKwh
	if ep.FloorOreKwh != nil && v < *ep.FloorOreKwh {
		v = *ep.FloorOreKwh
	}
	return v
}

// GridCostOre returns the net ore cost for a slot-level grid flow. Positive
// gridKWh imports at importOreKwh. Negative gridKWh exports at ExportPriceOre;
// if that export price is negative the returned cost is positive.
func GridCostOre(importOreKwh, spotOreKwh, gridKWh float64, ep ExportPricing) float64 {
	if gridKWh > 0 {
		return importOreKwh * gridKWh
	}
	return -ExportPriceOre(spotOreKwh, ep) * (-gridKWh)
}
