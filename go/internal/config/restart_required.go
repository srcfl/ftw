package config

import "reflect"

// RestartRequiredFor diffs old vs new and returns a list of human-readable
// section names whose changes are NOT picked up by the configreload watcher.
// An empty slice means the running process can safely apply `new` without a
// restart.
//
// The list mirrors what the applier in cmd/ftw/main.go already
// hot-reloads:
//
//   - Site control scalars (grid_target, tolerance, slew, min_dispatch),
//     fuse params, drivers, capacities, inverter groups, driver limits,
//     loadpoints, notifications, assistant, MPC capacity, the Weather subset
//     {pv_rated_w, latitude, longitude}, fleet_ping.enabled, and
//     home_assistant.* reload live.
//   - Everything else (api.port, state.path, price.*, planner.*, nova.*,
//     ev_charger.*, ocpp.*, modbus_proxy.*, weather.provider/arrays,
//     site.control_interval_s, site.watchdog_timeout_s, site.smoothing_alpha,
//     site.gain) needs the binary restarted to take effect.
//
// Keep this in sync with the applier whenever a section becomes
// hot-reloadable. The cost of forgetting is benign: an unnecessary
// "restart required" prompt to the operator. The cost of the inverse —
// telling them no restart is needed when one is — leaves the process
// running stale config silently, so when in doubt, list the section here.
func RestartRequiredFor(oldCfg, newCfg *Config) []string {
	if oldCfg == nil || newCfg == nil {
		return nil
	}
	var reasons []string

	// Site: only the four control scalars + fuse-related fields are
	// hot-reloaded. Anything else in Site changes the boot-time wiring.
	if oldCfg.Site.ControlIntervalS != newCfg.Site.ControlIntervalS {
		reasons = append(reasons, "site.control_interval_s — control loop tick rate is set at startup")
	}
	if oldCfg.Site.WatchdogTimeoutS != newCfg.Site.WatchdogTimeoutS {
		reasons = append(reasons, "site.watchdog_timeout_s — watchdog interval is captured at startup")
	}
	if oldCfg.Site.SmoothingAlpha != newCfg.Site.SmoothingAlpha {
		reasons = append(reasons, "site.smoothing_alpha — Kalman smoothing factor is fixed at startup")
	}
	if oldCfg.Site.Gain != newCfg.Site.Gain {
		reasons = append(reasons, "site.gain — PI controller gain is fixed at startup")
	}
	if oldCfg.Site.Name != newCfg.Site.Name {
		reasons = append(reasons, "site.name — used by HA discovery + logging at boot")
	}

	if oldCfg.API.Port != newCfg.API.Port {
		reasons = append(reasons, "api.port — HTTP server binds the port at startup")
	}
	reasons = append(reasons, modbusProxyRestartReasons(oldCfg, newCfg)...)
	// homeassistant.* is hot-reloadable via (*ha.Bridge).Reload; see the
	// applier in cmd/ftw/main.go.
	if !pointerEqual(oldCfg.State, newCfg.State) {
		reasons = append(reasons, "state — SQLite database paths are opened at startup")
	}
	if !pointerEqual(oldCfg.Price, newCfg.Price) {
		reasons = append(reasons, "price — spot-price service is constructed once at startup")
	}
	if !pointerEqual(oldCfg.Planner, newCfg.Planner) {
		reasons = append(reasons, "planner — MPC planner is constructed once at startup")
	}
	if !pointerEqual(oldCfg.Nova, newCfg.Nova) {
		reasons = append(reasons, "nova — federation client is constructed once at startup")
	}
	if oldCfg.AppLink.On() != newCfg.AppLink.On() {
		reasons = append(reasons, "app_link — the app uplink is connected at startup")
	}
	// fleet_ping.enabled is read at each send, so the switch takes effect at
	// once. The endpoint is resolved when the sender is built, so only that
	// field is worth a prompt — and it is compared resolved, or a posted
	// config with no fleet_ping section would look like a move away from the
	// default and prompt for a restart that changes nothing.
	if oldCfg.FleetPing.Resolved() != newCfg.FleetPing.Resolved() {
		reasons = append(reasons, "fleet_ping.endpoint — the sender resolves its endpoint at startup")
	}
	// The OCPP central system is started once in main.go; the config
	// applier neither starts, stops nor re-arms it. Without this entry
	// the Chargers panel's enable toggle saved cleanly, reported no
	// restart needed, and the listener never opened — the exact silent
	// failure the comment at the top of this file warns about.
	if !pointerEqual(oldCfg.OCPP, newCfg.OCPP) {
		reasons = append(reasons, "ocpp — the central system listener is started at startup")
	}
	if !pointerEqual(oldCfg.EVCharger, newCfg.EVCharger) {
		reasons = append(reasons, "ev_charger — EV charger client is constructed once at startup")
	}

	// Weather: PVRatedW, Latitude, Longitude reload live; everything else
	// (provider, arrays, tilt/azimuth, heating coefficient) is captured
	// once when the forecast + PV-twin services are wired up.
	if weatherNeedsRestart(oldCfg.Weather, newCfg.Weather) {
		reasons = append(reasons, "weather (provider / pv_arrays / tilt / azimuth / heating) — forecast + PV-twin wiring is set at startup")
	}

	return reasons
}

func pointerEqual(a, b any) bool {
	// reflect.DeepEqual already handles nil-vs-nil and value comparisons
	// correctly through interfaces, but two *T's pointing at zero-valued
	// structs and one *T-nil are intentionally distinct here: an operator
	// adding an empty `homeassistant: {}` block IS a change worth flagging.
	return reflect.DeepEqual(a, b)
}

func weatherNeedsRestart(oldW, newW *Weather) bool {
	if oldW == nil && newW == nil {
		return false
	}
	if oldW == nil || newW == nil {
		// Toggling weather on/off requires re-wiring forecast + PV twin.
		return true
	}
	if oldW.Provider != newW.Provider {
		return true
	}
	if oldW.PVTiltDeg != newW.PVTiltDeg || oldW.PVAzimuthDeg != newW.PVAzimuthDeg {
		return true
	}
	if oldW.HeatingWPerDegC != newW.HeatingWPerDegC {
		return true
	}
	if !reflect.DeepEqual(oldW.PVArrays, newW.PVArrays) {
		return true
	}
	// APIKey doesn't strictly require restart — the forecast service
	// reads it on each fetch — but mid-flight rotation is rare enough
	// that we flag it conservatively only when it goes from set→unset.
	if (oldW.APIKey != "") != (newW.APIKey != "") {
		return true
	}
	return false
}
