package flexload

// PlugProfile learns the electrical character of whatever is plugged into a
// smart plug / switch / contactor, so the deferrable scheduler doesn't have
// to be told the exact wattage and energy of a spa, water heater, pump, etc.
//
// It learns two things from the plug's own power metering:
//   - RunningW   — the typical power draw while the appliance is ON (an EMA
//     over samples above the on-threshold), so the scheduler knows how much
//     energy each slot it turns the plug on will actually deliver.
//   - DailyEnergyWh — a rolling estimate of the appliance's daily energy
//     appetite, so the scheduler can size its run budget when the operator
//     hasn't pinned one.
//
// Classify() turns the learned shape into a coarse device label for
// diagnostics/UI only — never for control decisions.
type PlugProfile struct {
	OnThresholdW  float64 `json:"on_threshold_w"`  // power above which we count "running"
	RunningW      float64 `json:"running_w"`       // EMA of power while running
	DailyEnergyWh float64 `json:"daily_energy_wh"` // EMA of per-day energy
	PeakW         float64 `json:"peak_w"`          // max observed running power
	Samples       int64   `json:"samples"`
	LastMs        int64   `json:"last_ms"`

	// Energy accumulation for the current UTC day, folded into
	// DailyEnergyWh at the day boundary.
	dayEnergyWh float64
	dayKey      int64 // UTC day number of the in-progress accumulation
}

// NewPlugProfile returns a fresh profile. onThresholdW defaults to 25 W if
// non-positive — below typical appliance standby, above plug self-draw.
func NewPlugProfile(onThresholdW float64) *PlugProfile {
	if onThresholdW <= 0 {
		onThresholdW = 25
	}
	return &PlugProfile{OnThresholdW: onThresholdW}
}

// Update folds one power sample (W) at nowMs into the profile, integrating
// energy over the elapsed time since the previous sample. dtMaxS bounds the
// integration step so a driver outage gap doesn't inject a huge energy slug.
func (p *PlugProfile) Update(powerW float64, nowMs int64, dtMaxS float64) {
	if powerW < 0 {
		powerW = 0
	}
	// Integrate energy since last sample.
	if p.LastMs != 0 {
		dt := float64(nowMs-p.LastMs) / 1000.0
		if dt > 0 && dt <= dtMaxS {
			dayKey := nowMs / 86_400_000
			if p.dayKey == 0 {
				p.dayKey = dayKey
			}
			if dayKey != p.dayKey {
				// Day rolled over — fold the completed day into the EMA.
				p.foldDay()
				p.dayKey = dayKey
			}
			p.dayEnergyWh += powerW * dt / 3600.0
		}
	}
	p.LastMs = nowMs

	// Learn running power only while clearly ON.
	if powerW >= p.OnThresholdW {
		if p.RunningW == 0 {
			p.RunningW = powerW
		} else {
			p.RunningW = 0.98*p.RunningW + 0.02*powerW
		}
		if powerW > p.PeakW {
			p.PeakW = powerW
		}
		p.Samples++
	}
}

// foldDay folds the in-progress day's accumulated energy into the rolling
// DailyEnergyWh EMA and resets the accumulator.
func (p *PlugProfile) foldDay() {
	if p.DailyEnergyWh == 0 {
		p.DailyEnergyWh = p.dayEnergyWh
	} else {
		p.DailyEnergyWh = 0.8*p.DailyEnergyWh + 0.2*p.dayEnergyWh
	}
	p.dayEnergyWh = 0
}

// EffectivePowerW returns the best estimate of run power: the operator's
// configured value when given, else the learned RunningW.
func (p *PlugProfile) EffectivePowerW(configuredW float64) float64 {
	if configuredW > 0 {
		return configuredW
	}
	return p.RunningW
}

// EffectiveEnergyWh returns the best estimate of the run budget: the
// operator's configured value when given, else the learned DailyEnergyWh.
func (p *PlugProfile) EffectiveEnergyWh(configuredWh float64) float64 {
	if configuredWh > 0 {
		return configuredWh
	}
	return p.DailyEnergyWh
}

// Classify returns a coarse device-type guess from the learned shape, for
// diagnostics only. Heuristics are intentionally conservative; "unknown" is
// returned until enough samples accumulate.
func (p *PlugProfile) Classify() string {
	if p.Samples < 200 || p.RunningW <= 0 {
		return "unknown"
	}
	switch {
	case p.RunningW >= 2500 && p.DailyEnergyWh >= 5000:
		return "water_heater" // high power, large daily energy
	case p.RunningW >= 1000 && p.DailyEnergyWh >= 3000:
		return "spa_or_pool" // sustained mid-high power, cyclic
	case p.RunningW < 1000 && p.DailyEnergyWh < 2000:
		return "small_appliance"
	default:
		return "general_load"
	}
}
