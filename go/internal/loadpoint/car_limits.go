package loadpoint

// WithCarLimits keeps the charger's wiring separate from the car's AC
// charger. A two-phase car uses two phases even on a three-phase site.
// An explicit one-phase choice remains available.
func WithCarLimits(cfg Config, phases int, maxW float64) Config {
	if phases == 2 && cfg.PhaseMode != "1p" && cfg.PhaseMode != "2p" {
		cfg.PhaseMode = "2p"
		cfg.MinChargeW *= 2.0 / 3
		// These steps describe the configured charger before the car's
		// phase count was known. Rebuild whole-amp steps for this car.
		cfg.AllowedStepsW = nil
	}
	if phases == 2 && finite(maxW) && maxW > 0 && (cfg.MaxChargeW <= 0 || maxW < cfg.MaxChargeW) {
		cfg.MaxChargeW = maxW
	}
	return cfg
}

func limitPhaseMode(mode, configured string) string {
	if configured == "2p" && mode != "1p" {
		return "2p"
	}
	return mode
}
