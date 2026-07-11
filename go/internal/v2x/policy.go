// Package v2x evaluates the operator policy envelope for bidirectional EV
// chargers. It does not send commands; dispatch remains in higher layers.
package v2x

import (
	"math"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

// Snapshot is the live state needed to answer whether V2X power is safe now.
type Snapshot struct {
	Driver string
	Online bool

	Connected *bool
	SoC       *float64 // 0..1

	CapacityWh         float64
	ChargePowerMaxW    float64
	DischargePowerMaxW float64
	RatedPowerW        float64

	// GridW is site convention: positive import, negative export. It is only
	// needed when export_allowed or grid_charging_allowed are false.
	GridW *float64

	Now time.Time
}

// Envelope is the policy result. MinPowerW is negative when discharge is
// allowed; MaxPowerW is positive when charge is allowed.
type Envelope struct {
	Driver string `json:"driver"`

	Enabled bool `json:"enabled"`

	MinPowerW     float64 `json:"min_power_w"`
	MaxPowerW     float64 `json:"max_power_w"`
	MaxChargeW    float64 `json:"max_charge_w"`
	MaxDischargeW float64 `json:"max_discharge_w"`
	ExportAllowed bool    `json:"export_allowed"`

	GridChargingAllowed bool `json:"grid_charging_allowed"`

	MinReserveSoCPct      float64  `json:"min_reserve_soc_pct,omitempty"`
	VehicleSoC            *float64 `json:"vehicle_soc,omitempty"`
	VehicleCapacityWh     float64  `json:"vehicle_capacity_wh,omitempty"`
	DepartureTargetSoCPct float64  `json:"departure_target_soc_pct,omitempty"`
	DepartureAt           string   `json:"departure_at,omitempty"`
	HoursToDeparture      float64  `json:"hours_to_departure,omitempty"`
	CycleCostOreKWh       float64  `json:"cycle_cost_ore_kwh,omitempty"`

	Reasons []string `json:"reasons,omitempty"`
}

// Evaluate returns the V2X power range allowed by policy for this instant.
// It deliberately collapses to 0 W when required safety inputs are stale or
// missing; future planner integration can only consume this envelope, not raw
// charger limits.
func Evaluate(policy *config.V2XPolicy, snap Snapshot) Envelope {
	now := snap.Now
	if now.IsZero() {
		now = time.Now()
	}
	env := Envelope{Driver: snap.Driver}
	if policy == nil || !policy.Enabled {
		env.addReason("policy_disabled")
		return env
	}

	env.Enabled = true
	env.ExportAllowed = policy.ExportAllowed
	env.GridChargingAllowed = policy.GridChargingAllowed
	env.MinReserveSoCPct = policy.MinReserveSoCPct
	env.VehicleCapacityWh = firstPositive(policy.VehicleCapacityWh, snap.CapacityWh)
	env.DepartureTargetSoCPct = policy.DepartureTargetSoCPct
	env.CycleCostOreKWh = policy.CycleCostOreKWh

	if policy.DriverName != "" && snap.Driver != policy.DriverName {
		env.addReason("driver_not_selected")
		return env
	}
	if !snap.Online {
		env.addReason("driver_offline")
		return env
	}
	if snap.Connected == nil {
		env.addReason("connected_unknown")
		return env
	}
	if !*snap.Connected {
		env.addReason("vehicle_disconnected")
		return env
	}
	if snap.SoC == nil {
		env.addReason("soc_missing")
		return env
	}
	soc := *snap.SoC
	if math.IsNaN(soc) || math.IsInf(soc, 0) || soc < 0 || soc > 1 {
		env.addReason("soc_invalid")
		return env
	}
	env.VehicleSoC = &soc

	chargeW := positiveMin(policy.MaxChargeW, snap.ChargePowerMaxW, snap.RatedPowerW)
	dischargeW := positiveMin(policy.MaxDischargeW, snap.DischargePowerMaxW, snap.RatedPowerW)
	if chargeW <= 0 {
		env.addReason("charge_limit_missing")
	}
	if dischargeW <= 0 {
		env.addReason("discharge_limit_missing")
	}

	reserve := policy.MinReserveSoCPct / 100.0
	if soc <= reserve {
		dischargeW = 0
		env.addReason("reserve_floor")
	}

	if policy.DepartureTargetSoCPct > 0 {
		departureAt, ok := nextDeparture(now, policy.DepartureTime)
		if ok {
			env.DepartureAt = departureAt.Format(time.RFC3339)
			hours := math.Max(0, departureAt.Sub(now).Hours())
			env.HoursToDeparture = hours
			if env.VehicleCapacityWh <= 0 {
				dischargeW = 0
				env.addReason("capacity_missing")
			} else {
				target := policy.DepartureTargetSoCPct / 100.0
				// recoverableW is the charge power we can *guarantee* before
				// departure. When grid charging is allowed it's the full
				// charger limit. When it isn't, the car can only recharge from
				// PV surplus, so crediting the full chargeW would understate
				// requiredNow and release discharge too early. The snapshot
				// only carries an instantaneous GridW (no forecast), so the
				// conservative bound is: zero guaranteed recharge unless there
				// is current surplus, in which case credit at most the surplus.
				recoverableW := chargeW
				if !policy.GridChargingAllowed {
					recoverableW = 0
					if snap.GridW != nil && *snap.GridW < 0 {
						surplus := -*snap.GridW
						if surplus < chargeW {
							recoverableW = surplus
						} else {
							recoverableW = chargeW
						}
					}
				}
				recoverableSoC := 0.0
				if hours > 0 && recoverableW > 0 {
					recoverableSoC = recoverableW * hours / env.VehicleCapacityWh
				}
				requiredNow := clamp01(target - recoverableSoC)
				if requiredNow < reserve {
					requiredNow = reserve
				}
				if soc <= requiredNow {
					dischargeW = 0
					env.addReason("departure_target_floor")
				}
				if soc < requiredNow {
					env.addReason("departure_target_at_risk")
				}
			}
		} else {
			dischargeW = 0
			env.addReason("departure_time_invalid")
		}
	}

	if !policy.ExportAllowed {
		if snap.GridW == nil {
			dischargeW = 0
			env.addReason("export_limit_unknown")
		} else if *snap.GridW <= 0 {
			dischargeW = 0
			env.addReason("export_blocked")
		} else if dischargeW > *snap.GridW {
			dischargeW = *snap.GridW
			env.addReason("export_limited_to_import")
		}
	}

	if !policy.GridChargingAllowed {
		if snap.GridW == nil {
			chargeW = 0
			env.addReason("grid_charge_limit_unknown")
		} else if *snap.GridW >= 0 {
			chargeW = 0
			env.addReason("grid_charging_blocked")
		} else if chargeW > -*snap.GridW {
			chargeW = -*snap.GridW
			env.addReason("charge_limited_to_surplus")
		}
	}

	if math.Abs(chargeW) < 1 {
		chargeW = 0
	}
	if math.Abs(dischargeW) < 1 {
		dischargeW = 0
	}
	env.MaxChargeW = chargeW
	env.MaxDischargeW = dischargeW
	env.MaxPowerW = chargeW
	env.MinPowerW = -dischargeW
	return env
}

func (e *Envelope) addReason(reason string) {
	for _, existing := range e.Reasons {
		if existing == reason {
			return
		}
	}
	e.Reasons = append(e.Reasons, reason)
}

func positiveMin(values ...float64) float64 {
	out := 0.0
	for _, v := range values {
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if out == 0 || v < out {
			out = v
		}
	}
	return out
}

func firstPositive(values ...float64) float64 {
	for _, v := range values {
		if v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return v
		}
	}
	return 0
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func nextDeparture(now time.Time, value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, true
	}
	clock, err := time.Parse("15:04", value)
	if err != nil {
		return time.Time{}, false
	}
	departure := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
	if !departure.After(now) {
		departure = departure.Add(24 * time.Hour)
	}
	return departure, true
}
