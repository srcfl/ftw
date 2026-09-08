package control

import "time"

func (d SlotDirective) priceSlotStart() time.Time {
	if !d.PriceSlotStart.IsZero() {
		return d.PriceSlotStart
	}
	return d.SlotStart
}

func (d SlotDirective) averageBatteryW() float64 {
	hours := d.SlotEnd.Sub(d.SlotStart).Hours()
	if hours <= 0 {
		return 0
	}
	return d.BatteryEnergyWh / hours
}

// Intent keeps the full price interval's threshold even when its remaining
// execution window is short. A 2 kW charge for five seconds is still a charge.
func (d SlotDirective) intentEnergyWh() float64 {
	if d.PriceSlotStart.IsZero() {
		return d.BatteryEnergyWh
	}
	return d.averageBatteryW() * d.SlotEnd.Sub(d.priceSlotStart()).Hours()
}

func elapsedDirectiveEnergy(d SlotDirective, now time.Time) float64 {
	if d.PriceSlotStart.IsZero() || !d.SlotStart.After(d.PriceSlotStart) {
		return 0
	}
	elapsed := now.Sub(d.SlotStart)
	if elapsed <= 0 {
		return 0
	}
	if now.After(d.SlotEnd) {
		return d.BatteryEnergyWh
	}
	return d.averageBatteryW() * elapsed.Hours()
}
