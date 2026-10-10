package telemetry

import (
	"encoding/json"
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/units"
)

// VehicleMaxAge is the freshness window past which a DerVehicle reading
// is considered stale enough to ignore for control decisions. Picked
// conservatively at 5 min so a vehicle driver that has lost contact
// (asleep car, paired-proxy outage, cloud-API throttle) cannot keep an
// old SoC live as ground truth. Tighter than this would churn against
// vendors whose backends only refresh on a 60–120 s cadence; looser
// would mean acting on a value that no longer reflects reality.
const VehicleMaxAge = 5 * time.Minute

// VehicleAnchorMaxAge bounds how old a car reading may be and still anchor
// a loadpoint's SoC estimate. The loadpoint adds the energy delivered since
// the reading, so age adds only metering error. Cloud sources such as the
// VW Group portal report every 15 minutes.
const VehicleAnchorMaxAge = time.Hour

// VehiclePick is the "best matching" DerVehicle reading for a loadpoint:
// the one most likely to be the car physically connected right now.
// Empty Driver means "no usable reading" — the caller should fall back
// to whatever inferred SoC was already in place.
type VehiclePick struct {
	Driver        string
	SoC           float64 // bounded [0,1]
	ChargeLimit   float64 // bounded [0,1]
	ChargingState string
	Stale         bool      // unsuitable as a live observation; may still anchor an estimate
	UpdatedAt     time.Time // wall-clock of the last fresh SoC observation
}

// VehicleConnectedRank scores how likely a DerVehicle driver is to be
// the one physically plugged into the loadpoint right now, using the
// charging_state vocabulary every vehicle driver normalizes to (the
// strings below are the canonical values; vendor specifics are
// translated inside each Lua driver). Higher rank = more likely
// connected. Negative = explicitly not connected; caller should skip.
//
// Single source of truth for the rank table — both main.go (MPC plan
// inputs) and api.go (loadpoint decoration) call this so multi-vehicle
// pick decisions stay consistent.
func VehicleConnectedRank(chargingState string) int {
	switch chargingState {
	case "Charging", "Starting":
		return 3 // actively pulling power — definitely this car
	case "NoPower":
		return 2 // plugged but wallbox not delivering yet
	case "Stopped", "Complete":
		return 1 // plugged + idle (charge limit reached, paused, etc.)
	case "Disconnected":
		return -1 // explicitly unplugged — never pick this one
	default:
		return 0 // unknown/missing — usable but de-prioritised
	}
}

// PickBestVehicle scans the store for the single DerVehicle reading
// most likely to be the car connected right now: highest
// VehicleConnectedRank, tiebreak by freshness. Returns a zero-value
// VehiclePick if no usable reading exists.
//
// Defenses applied here (do NOT skip — every vehicle driver pulls
// from a network trust boundary, whether a local BLE proxy, an
// in-LAN OEM gateway, or a cloud API):
//   - SoC bounded to [0,1] — a misbehaving driver reporting 2.0 or
//     -0.5 must not be able to overcharge or freeze EV charging.
//   - ChargeLimit bounded to [0,1] — same risk.
//   - Stale by `now − SoCUpdatedAt > VehicleMaxAge` — wallclock check on
//     the last fresh SoC observation, even when newer power or metadata
//     updates carry the last-known value forward. A driver that stops
//     publishing SoC mustn't keep it live forever.
//   - Explicit `stale=true` — a driver that knows its upstream value is stale
//     can stop control from using it before the wallclock limit.
//   - Driver health-online check — offline drivers contribute nothing.
//
// Lives in telemetry/ rather than api/ or cmd/ because both packages
// need it and the dependency direction otherwise cycles.
func PickBestVehicle(s *Store, now time.Time) VehiclePick {
	return pickBestVehicle(s, 0, now, VehicleMaxAge, vehiclePickLive)
}

// PickBestVehicleForLoadpoint adds connection-evidence gating: when
// the loadpoint is delivering power right now (current_power_w over
// the threshold), the picker requires the vehicle's charging_state
// to be Charging or Starting (rank 3). Any other state — including
// Stopped/Complete on a vehicle parked elsewhere — is rejected, so
// a second car returning SoC from outside this charger cannot win
// the pick on freshness alone.
//
// When the loadpoint is plugged but idle (no current draw), gating
// falls back to the standard rank-based pick. We don't have strong
// evidence which car is connected during idle, but the planner is
// also not actively committing power, so a wrong pick during this
// window is much lower-impact than during active delivery.
func PickBestVehicleForLoadpoint(s *Store, lpDeliveringPower bool, now time.Time) VehiclePick {
	minRank := 0 // any non-Disconnected
	if lpDeliveringPower {
		// Strict: only Charging/Starting count as evidence the vehicle
		// is on this charger. A vehicle reporting Stopped while another
		// loadpoint is at 11 kW is definitely not the connected one.
		minRank = 3
	}
	return pickBestVehicle(s, minRank, now, VehicleMaxAge, vehiclePickLive)
}

// PickVehicleForAnchor applies the loadpoint gates with VehicleAnchorMaxAge.
// A cached cloud report with a measured age may also supply a historical
// anchor. Its SoC is a past observation: anchor it at UpdatedAt, never as now.
// The loadpoint must still know the session's energy at that time.
func PickVehicleForAnchor(s *Store, lpDeliveringPower bool, now time.Time) VehiclePick {
	minRank := 0
	if lpDeliveringPower {
		minRank = 3
	}
	return pickBestVehicle(s, minRank, now, VehicleAnchorMaxAge, vehiclePickAnchor)
}

// PickBestVehicleForDisplay retains an otherwise valid last-known vehicle
// observation after the five-minute control freshness window. The returned
// pick is marked stale, so presentation can show provenance and age without
// making the old SoC usable by MPC or control.
func PickBestVehicleForDisplay(s *Store, lpDeliveringPower bool, now time.Time) VehiclePick {
	minRank := 0
	if lpDeliveringPower {
		minRank = 3
	}
	return pickBestVehicle(s, minRank, now, VehicleMaxAge, vehiclePickDisplay)
}

// PickVehicleForCompletion requires one vehicle source. Rank and freshness
// cannot bind one of several cars to a charger or prove that its goal is done.
// Callers must also check that only one loadpoint is connected.
func PickVehicleForCompletion(s *Store, now time.Time) VehiclePick {
	if s == nil || len(s.ReadingsByType(DerVehicle)) != 1 {
		return VehiclePick{}
	}
	return pickBestVehicle(s, 1, now, VehicleMaxAge, vehiclePickLive)
}

type vehiclePickUse uint8

const (
	vehiclePickLive vehiclePickUse = iota
	vehiclePickAnchor
	vehiclePickDisplay
)

func pickBestVehicle(s *Store, minRank int, now time.Time, maxAge time.Duration, use vehiclePickUse) VehiclePick {
	if s == nil {
		return VehiclePick{}
	}
	var best VehiclePick
	bestRank := -1
	allowAgeStale := use == vehiclePickDisplay
	for _, vr := range s.ReadingsByType(DerVehicle) {
		socValue := vr.SoC
		socUpdatedAt := vr.SoCUpdatedAt
		cachedObservation := false
		if allowAgeStale || use == vehiclePickAnchor {
			if value, observedAt, ok := cachedVehicleObservation(s, vr); ok &&
				(allowAgeStale || socUpdatedAt.IsZero() || !observedAt.Before(socUpdatedAt)) {
				socValue = &value
				socUpdatedAt = observedAt
				cachedObservation = true
			}
		}
		if socValue == nil {
			continue
		}
		if h := s.DriverHealth(vr.Driver); h == nil || !h.IsOnline() {
			continue
		}
		if socUpdatedAt.IsZero() {
			continue
		}
		age := now.Sub(socUpdatedAt)
		if age < 0 {
			continue
		}
		if age > maxAge && !allowAgeStale {
			// Reading is older than we're willing to trust as ground
			// truth — driver probably stopped publishing. Skip rather
			// than risk acting on a stale SoC.
			continue
		}
		var meta struct {
			ChargingState  string  `json:"charging_state"`
			ChargeLimit    float64 `json:"charge_limit"`
			ChargeLimitPct float64 `json:"charge_limit_pct"`
			Stale          bool    `json:"stale"`
		}
		if len(vr.Data) > 0 {
			_ = json.Unmarshal(vr.Data, &meta)
		}
		if meta.Stale && !allowAgeStale && !(use == vehiclePickAnchor && cachedObservation) {
			continue
		}
		rank := VehicleConnectedRank(meta.ChargingState)
		if rank < 0 || rank < minRank {
			continue
		}
		if rank < bestRank || (rank == bestRank && !socUpdatedAt.After(best.UpdatedAt)) {
			continue
		}
		soc := units.ClampFraction(*socValue)
		limit := units.ClampFraction(units.DecodeJSONFraction(meta.ChargeLimit, meta.ChargeLimitPct))
		best = VehiclePick{
			Driver:        vr.Driver,
			SoC:           soc,
			ChargeLimit:   limit,
			ChargingState: meta.ChargingState,
			Stale:         age > VehicleMaxAge || meta.Stale || cachedObservation,
			UpdatedAt:     socUpdatedAt,
		}
		bestRank = rank
	}
	return best
}

// Cached car reports can arrive before any fresh report after a restart.
// Store.SoC intentionally retains only control-trusted observations. Read the
// latest raw report with its explicit cache flag and measured SoC age. It may
// be displayed or anchored in a session with known metering history, but must
// never become a live observation or confirm completion. Never synthesize a
// fresh receipt time.
func cachedVehicleObservation(s *Store, vr *DerReading) (float64, time.Time, bool) {
	var report struct {
		SoC      *float64 `json:"soc"`
		SoCFresh *bool    `json:"soc_fresh"`
	}
	if json.Unmarshal(vr.Data, &report) != nil || report.SoC == nil ||
		report.SoCFresh == nil || *report.SoCFresh {
		return 0, time.Time{}, false
	}
	raw := *report.SoC
	if math.IsNaN(raw) || math.IsInf(raw, 0) || raw < 0 || raw > 100 {
		return 0, time.Time{}, false
	}
	age, receivedAt, ok := s.LatestMetric(vr.Driver, "vehicle_soc_age_s")
	if !ok || receivedAt.IsZero() || math.IsNaN(age) || math.IsInf(age, 0) ||
		age < 0 || age > (100*365*24*time.Hour).Seconds() {
		return 0, time.Time{}, false
	}
	return units.DecodeJSONFraction(0, raw), receivedAt.Add(-time.Duration(age * float64(time.Second))), true
}
