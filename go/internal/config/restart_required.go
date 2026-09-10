package config

import "reflect"

// RestartRequiredFor diffs old vs new and returns a list of human-readable
// section names whose changes are NOT picked up by the configreload watcher.
// An empty slice means the running process can safely apply `new` without a
// restart.
//
// Restart is only for process wiring that is constructed once: listen
// sockets, database files, price/planner/nova/OCPP/EV clients. Ordinary
// operator settings (retention, gain, tick rate, watchdog, weather
// geometry, price tariff, planner SoC) apply live. Keep this in sync
// with cmd/ftw/main.go's applier. When in doubt, list the section —
// a stale silent process is worse than an extra prompt.
func RestartRequiredFor(oldCfg, newCfg *Config) []string {
	if oldCfg == nil || newCfg == nil {
		return nil
	}
	var reasons []string

	if oldCfg.API.Port != newCfg.API.Port {
		reasons = append(reasons, "api.port — HTTP server binds the port at startup")
	}
	if stateNeedsRestart(oldCfg.State, newCfg.State) {
		reasons = append(reasons, "state.path / state.cold_dir — database files are opened at startup")
	}
	if priceNeedsRestart(oldCfg.Price, newCfg.Price) {
		reasons = append(reasons, "price.provider / zone / API key — spot-price client is constructed at startup")
	}
	if plannerNeedsRestart(oldCfg.Planner, newCfg.Planner) {
		reasons = append(reasons, "planner.enabled / engine — planner process is constructed at startup")
	}
	if !pointerEqual(oldCfg.Nova, newCfg.Nova) {
		reasons = append(reasons, "nova — federation client is constructed once at startup")
	}
	if oldCfg.AppLink.On() != newCfg.AppLink.On() {
		reasons = append(reasons, "app_link — the app uplink is connected at startup")
	}
	if oldCfg.FleetPing.Resolved() != newCfg.FleetPing.Resolved() {
		reasons = append(reasons, "fleet_ping.endpoint — the sender resolves its endpoint at startup")
	}
	if !pointerEqual(oldCfg.OCPP, newCfg.OCPP) {
		reasons = append(reasons, "ocpp — the central system listener is started at startup")
	}
	if !pointerEqual(oldCfg.EVCharger, newCfg.EVCharger) {
		reasons = append(reasons, "ev_charger — EV charger client is constructed once at startup")
	}
	if weatherNeedsRestart(oldCfg.Weather, newCfg.Weather) {
		reasons = append(reasons, "weather.provider — forecast service is started when weather is first enabled")
	}

	return reasons
}

func pointerEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

func stateNeedsRestart(oldS, newS *StateConf) bool {
	var oldPath, newPath, oldCold, newCold string
	if oldS != nil {
		oldPath, oldCold = oldS.Path, oldS.ColdDir
	}
	if newS != nil {
		newPath, newCold = newS.Path, newS.ColdDir
	}
	return oldPath != newPath || oldCold != newCold
}

func priceNeedsRestart(oldP, newP *Price) bool {
	oldOn := oldP != nil && oldP.Provider != "" && oldP.Provider != "none"
	newOn := newP != nil && newP.Provider != "" && newP.Provider != "none"
	if oldOn != newOn {
		return true
	}
	if !oldOn {
		return false
	}
	return oldP.Provider != newP.Provider || oldP.Zone != newP.Zone || oldP.APIKey != newP.APIKey || oldP.Currency != newP.Currency
}

func plannerNeedsRestart(oldP, newP *Planner) bool {
	oldOn := oldP != nil && oldP.Enabled
	newOn := newP != nil && newP.Enabled
	if oldOn != newOn {
		return true
	}
	if !oldOn {
		return false
	}
	return oldP.EngineName() != newP.EngineName()
}

func weatherNeedsRestart(oldW, newW *Weather) bool {
	oldOn := oldW != nil && oldW.Provider != "" && oldW.Provider != "none"
	newOn := newW != nil && newW.Provider != "" && newW.Provider != "none"
	// Off → on needs a Service that was never started. On → off and
	// provider swaps go through forecast.Service.Reconfigure.
	return !oldOn && newOn
}
