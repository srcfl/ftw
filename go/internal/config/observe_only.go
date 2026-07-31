package config

// ObserveOnlyDriverSet returns the set of configured driver names marked
// observe_only. Callers use it to skip dispatch commands, watchdog
// DefaultMode, and other actuation paths while still accepting telemetry.
func ObserveOnlyDriverSet(cfg *Config) map[string]bool {
	out := make(map[string]bool)
	if cfg == nil {
		return out
	}
	for _, d := range cfg.Drivers {
		if d.ObserveOnly {
			out[d.Name] = true
		}
	}
	return out
}
