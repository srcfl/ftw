package control

import (
	"maps"
	"math"
	"time"
)

type storageSlotDelivery struct {
	start, end, tick  time.Time
	decision          string
	budget, delivered map[string]float64
}

// Keep each physical battery's share after Core has computed a live site
// target. A blocked battery's energy is not reassigned to another battery.
// Manual control and missing plans retain the existing local controller.
func distributePlannedStorages(state *State, bats []batteryInfo, desired float64, manual bool) ([]DispatchTarget, bool) {
	if manual || !state.Mode.IsPlannerMode() || state.PlanStale {
		state.storageDelivery = storageSlotDelivery{}
		return nil, false
	}
	now := state.now()
	dir, ok := plannerSelfDirectiveAt(state, now)
	if !ok || len(dir.StorageEnergyWh) < 2 {
		state.storageDelivery = storageSlotDelivery{}
		return nil, false
	}
	allIdle := true
	for _, wh := range dir.StorageEnergyWh {
		allIdle = allIdle && math.Abs(wh) < 1e-6
	}
	if allIdle {
		state.storageDelivery = storageSlotDelivery{}
		return nil, false
	}
	d := &state.storageDelivery
	slotS := dir.SlotEnd.Sub(dir.SlotStart).Seconds()
	if slotS <= 0 || now.Before(dir.SlotStart) || !now.Before(dir.SlotEnd) {
		return nil, false
	}
	elapsed := math.Max(0, math.Min(1, now.Sub(dir.SlotStart).Seconds()/slotS))
	reset := !d.start.Equal(dir.SlotStart) || d.tick.IsZero() || now.Sub(d.tick) >= 5*time.Minute
	if reset {
		*d = storageSlotDelivery{start: dir.SlotStart, delivered: make(map[string]float64)}
		// Starting partway through a slot must not demand all its energy in
		// the remaining seconds. Replanning already starts from measured SoC.
		for id, wh := range dir.StorageEnergyWh {
			d.delivered[id] = wh * elapsed
		}
	} else {
		dt := now.Sub(d.tick).Hours()
		if dt > 0 {
			for _, b := range bats {
				if _, exists := d.budget[b.driver]; exists {
					d.delivered[b.driver] += b.currentW * dt
				}
			}
		}
		if d.decision != dir.DecisionID || !maps.Equal(d.budget, dir.StorageEnergyWh) || !d.end.Equal(dir.SlotEnd) {
			for id, wh := range dir.StorageEnergyWh {
				actual := d.delivered[id]
				if _, exists := d.budget[id]; !exists || wh*d.budget[id] < 0 {
					actual = wh * elapsed
				} else if wh > 0 {
					actual = math.Max(actual, wh*elapsed)
				} else if wh < 0 {
					actual = math.Min(actual, wh*elapsed)
				}
				d.delivered[id] = actual
			}
		}
	}
	d.end, d.tick, d.decision = dir.SlotEnd, now, dir.DecisionID
	d.budget = maps.Clone(dir.StorageEnergyWh)
	remainingS := dir.SlotEnd.Sub(now).Seconds()
	targets := make([]float64, len(bats))
	var sum float64
	for i, b := range bats {
		wh := d.budget[b.driver]
		remaining := wh - d.delivered[b.driver]
		// Completion cannot reverse a slot's intended direction.
		if wh > 0 {
			remaining = math.Max(0, remaining)
		} else if wh < 0 {
			remaining = math.Min(0, remaining)
		} else {
			remaining = 0
		}
		w := 0.0
		if remainingS > .5 {
			w = remaining * 3600 / remainingS
		}
		if w*desired <= 0 || (w > 0 && b.chargeBlocked) || (w < 0 && b.dischargeBlocked) {
			w = 0
		}
		targets[i], _ = clampWithSoC(w, b)
		sum += targets[i]
	}
	scale := 0.0
	if sum*desired > 0 {
		scale = math.Min(1, math.Abs(desired/sum))
	}
	out := make([]DispatchTarget, 0, len(bats))
	for i, b := range bats {
		out = append(out, DispatchTarget{Driver: b.driver, TargetW: targets[i] * scale, Clamped: true})
	}
	return out, true
}
