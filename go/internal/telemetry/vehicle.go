package telemetry

import (
	"encoding/json"
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

// VehiclePick is the "best matching" DerVehicle reading for a loadpoint:
// the one most likely to be the car physically connected right now.
// Empty Driver means "no usable reading" — the caller should fall back
// to whatever inferred SoC was already in place.
type VehiclePick struct {
	Driver        string
	SoC           float64 // bounded [0,1]
	ChargeLimit   float64 // bounded [0,1]
	ChargingState string
	Stale         bool      // false for usable picks; retained in the result shape
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
	return pickBestVehicle(s, 0, now)
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
	return pickBestVehicle(s, minRank, now)
}

func pickBestVehicle(s *Store, minRank int, now time.Time) VehiclePick {
	if s == nil {
		return VehiclePick{}
	}
	var best VehiclePick
	bestRank := -1
	for _, vr := range s.ReadingsByType(DerVehicle) {
		if vr.SoC == nil {
			continue
		}
		if h := s.DriverHealth(vr.Driver); h == nil || !h.IsOnline() {
			continue
		}
		socUpdatedAt := vr.SoCUpdatedAt
		if socUpdatedAt.IsZero() || now.Sub(socUpdatedAt) > VehicleMaxAge {
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
		if meta.Stale {
			continue
		}
		rank := VehicleConnectedRank(meta.ChargingState)
		if rank < 0 || rank < minRank {
			continue
		}
		if rank < bestRank || (rank == bestRank && !socUpdatedAt.After(best.UpdatedAt)) {
			continue
		}
		soc := units.ClampFraction(*vr.SoC)
		limit := units.ClampFraction(units.DecodeJSONFraction(meta.ChargeLimit, meta.ChargeLimitPct))
		best = VehiclePick{
			Driver:        vr.Driver,
			SoC:           soc,
			ChargeLimit:   limit,
			ChargingState: meta.ChargingState,
			Stale:         meta.Stale,
			UpdatedAt:     socUpdatedAt,
		}
		bestRank = rank
	}
	return best
}
