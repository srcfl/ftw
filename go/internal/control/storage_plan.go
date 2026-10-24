package control

import (
	"maps"
	"math"
	"slices"
	"time"
)

type storageSlotDelivery struct {
	start, end, tick  time.Time
	decision          string
	budget, delivered map[string]float64
}

// Keep each physical battery's share after Core has computed a live site
// target. A blocked battery's energy is not reassigned to another battery.
// Interchangeable batteries share the live target by available energy
// instead. Manual control, a plan without per-battery energy, an idle slot,
// a single online battery and interchangeable batteries without a known SoC
// retain the existing local controller, which splits by capacity.
func distributePlannedStorages(state *State, bats []batteryInfo, desired float64, manual bool) ([]DispatchTarget, bool) {
	if manual || !state.Mode.IsPlannerMode() || state.PlanStale || len(bats) < 2 {
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
	if interchangeableStorages(bats) {
		state.storageDelivery = storageSlotDelivery{}
		if slices.ContainsFunc(bats, func(b batteryInfo) bool { return b.socMissing }) {
			return nil, false
		}
		return shareByAvailableEnergy(bats, desired), true
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

// Batteries with the same charge and discharge power per Wh of capacity are
// one pool to the planner. Core gives every storage the site's efficiencies,
// so each Wh costs the same on either battery, and a split by capacity can
// carry any total the plan can. Which battery the plan names is then
// arbitrary: Energyplan often puts a whole slot on one and the next on the
// other. A 5 % spread costs a capacity split at most about 1.3 % of the
// fleet's full power.
const interchangeablePowerSpread = 0.05

func interchangeableStorages(bats []batteryInfo) bool {
	if len(bats) == 0 {
		return false
	}
	lowC, highC := math.Inf(1), math.Inf(-1)
	lowD, highD := math.Inf(1), math.Inf(-1)
	for _, b := range bats {
		if b.capacityWh <= 0 {
			return false
		}
		c, d := b.chargeCap()/b.capacityWh, b.dischargeCap()/b.capacityWh
		lowC, highC = math.Min(lowC, c), math.Max(highC, c)
		lowD, highD = math.Min(lowD, d), math.Max(highD, d)
	}
	return highC-lowC <= interchangeablePowerSpread*highC &&
		highD-lowD <= interchangeablePowerSpread*highD
}

// Split desired across interchangeable batteries by the energy each can still
// give (above the discharge floor) or take (below full). The fuller battery
// discharges faster and the emptier one charges faster, so a SoC gap shrinks
// while they work. A blocked battery gets nothing; a battery at its power
// limit stays there and the others take the rest.
func shareByAvailableEnergy(bats []batteryInfo, desired float64) []DispatchTarget {
	out := make([]DispatchTarget, len(bats))
	room := make([]float64, len(bats))
	for i, b := range bats {
		out[i].Driver = b.driver
		if desired < 0 && !b.dischargeBlocked {
			room[i] = b.capacityWh * math.Max(0, b.soc-dischargeFloorSoC)
		} else if desired > 0 && !b.chargeBlocked {
			room[i] = b.capacityWh * math.Max(0, 1-b.soc)
		}
		// Blocked, empty or full: this battery cannot take part.
		out[i].Clamped = desired != 0 && room[i] <= 0
	}
	left := math.Abs(desired)
	for left > 0 {
		var total, pinnedW float64
		for _, r := range room {
			total += r
		}
		if total <= 0 {
			break
		}
		pinned := false
		for i, b := range bats {
			limit := b.chargeCap()
			if desired < 0 {
				limit = b.dischargeCap()
			}
			if room[i] > 0 && left*room[i]/total > limit {
				out[i].TargetW = math.Copysign(limit, desired)
				out[i].Clamped = true
				pinnedW += limit
				room[i] = 0
				pinned = true
			}
		}
		if !pinned {
			for i := range bats {
				if room[i] > 0 {
					out[i].TargetW = math.Copysign(left*room[i]/total, desired)
				}
			}
			break
		}
		left -= pinnedW
	}
	for i, b := range bats {
		var clamped bool
		out[i].TargetW, clamped = clampWithSoC(out[i].TargetW, b)
		out[i].Clamped = out[i].Clamped || clamped
	}
	return out
}
