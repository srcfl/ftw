package control

import (
	"sort"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// SolarFeedTarget is one per-tick solar-surplus hint for a driver whose
// device consumes an "available solar power" input (e.g. the NIBE
// S-series Solar PV registers, driver action `solar_pv`). PowerW is
// site-signed like every power in core: negative = power leaving the
// site = exportable surplus; 0 = no surplus right now. The driver owns
// the device-side safety (clamp to its configured ceiling, deadband,
// rate limit, dead-man's switch), so the hint is sent every tick — a
// value repeated unchanged is what keeps the driver's dead-man fed.
type SolarFeedTarget struct {
	Driver string  `json:"driver"`
	PowerW float64 `json:"power_w"`
}

// ComputeSolarFeed returns one SolarFeedTarget per driver in
// state.SolarFeedDrivers for this dispatch tick. Drivers not in the set
// are silently skipped — only a driver whose operator armed its write
// path (config write gates, see main.go's solarFeedDriversFrom) may
// receive a `solar_pv` payload.
//
// Every listed driver gets the same site-wide value: solar surplus is a
// property of the site boundary, not of the consuming device. The caller
// dispatches behind the site-meter freshness gate, and a stale gate
// already reverts every driver to its default mode (which clears the
// device-side feed), so this function does not re-check freshness.
func ComputeSolarFeed(state *State, store *telemetry.Store) []SolarFeedTarget {
	if state == nil || store == nil {
		return nil
	}
	names := make([]string, 0, len(state.SolarFeedDrivers))
	for name, on := range state.SolarFeedDrivers {
		if on {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)

	surplus := solarSurplusW(state, store)
	out := make([]SolarFeedTarget, 0, len(names))
	for _, name := range names {
		out = append(out, SolarFeedTarget{Driver: name, PowerW: -surplus})
	}
	return out
}

// solarSurplusW is the watts of live grid export attributable to solar:
//
//	min(online PV generation, export − battery/V2X discharge), floored at 0.
//
// The two clamps each guard a real misattribution:
//
//   - capping at PV generation keeps a site that exports without sun
//     (battery arbitrage, V2X) from advertising those watts as free
//     solar the device would then burn against the owner's arbitrage;
//   - subtracting battery/V2X discharge first removes the storage
//     contribution from mixed export, so 4 kW export during a 3 kW
//     battery discharge reports only the 1 kW the sun is covering.
//
// Only online drivers count. Telemetry store signs: sources are negative
// (PV generating, meter exporting, battery/V2X discharging), so
// generation and discharge are read as -SmoothedW. Missing telemetry
// degrades toward 0 — the safe direction for a hint whose worst case is
// a device soaking surplus that is not there.
func solarSurplusW(state *State, store *telemetry.Store) float64 {
	if state.SiteMeterDriver == "" {
		return 0
	}
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	if meter == nil {
		return 0
	}
	exportW := -meter.SmoothedW
	if exportW <= 0 {
		return 0
	}

	pvGenW := sumOnlineSourcedW(store, telemetry.DerPV)
	if pvGenW <= 0 {
		return 0
	}

	exportW -= sumOnlineSourcedW(store, telemetry.DerBattery)
	exportW -= sumOnlineSourcedW(store, telemetry.DerV2X)
	if exportW <= 0 {
		return 0
	}
	if pvGenW < exportW {
		return pvGenW
	}
	return exportW
}

// sumOnlineSourcedW sums the power currently sourced (negative
// SmoothedW) by online drivers of one DER type, as a positive number.
// Consuming readings (charging battery, curtailed-to-zero PV) add 0.
func sumOnlineSourcedW(store *telemetry.Store, typ telemetry.DerType) float64 {
	var sum float64
	for _, r := range store.ReadingsByType(typ) {
		h := store.DriverHealth(r.Driver)
		if h == nil || !h.IsOnline() {
			continue
		}
		if r.SmoothedW < 0 {
			sum += -r.SmoothedW
		}
	}
	return sum
}
