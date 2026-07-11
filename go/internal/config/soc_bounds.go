package config

// WithBatterySoCBounds returns a copy of drivers in which every driver that has
// a matching batteries[<name>] entry gets charge_ceil_soc / discharge_floor_soc
// defaulted into its free-form Config from the battery's soc_max / soc_min.
//
// This makes batteries.<name>.soc_max the single source of truth for a
// battery's usable SoC window. The planner (MPC) already reads soc_max; the
// driver layer did not. So a driver's built-in ceiling — e.g. the Ferroamp
// driver's CHARGE_CEIL_SOC default of 0.95 — silently overrode an operator's
// `soc_max: 1.0`: dispatch kept asking for charge but the driver reported its
// pack "full" at ~95% and idled, so the battery never reached 100%. Routing
// soc_max through as charge_ceil_soc removes the second, hidden ceiling.
//
// An explicit non-null key already present in the driver's `config:` block
// always wins (operator override). A YAML null is treated as unset so it cannot
// accidentally suppress the battery-level bound. Inputs are never mutated — a driver receives a fresh
// Config map only when a value is actually injected — so the persisted config
// is not polluted with derived keys, and a later soc_max change is never
// shadowed by a stale, persisted charge_ceil_soc.
func WithBatterySoCBounds(drivers []Driver, batteries map[string]Battery) []Driver {
	out := make([]Driver, len(drivers))
	copy(out, drivers)
	for i := range out {
		b, ok := batteries[out[i].Name]
		if !ok {
			continue
		}
		inject := map[string]float64{}
		if b.SoCMax != nil {
			if v, exists := out[i].Config["charge_ceil_soc"]; !exists || v == nil {
				inject["charge_ceil_soc"] = *b.SoCMax
			}
		}
		if b.SoCMin != nil {
			if v, exists := out[i].Config["discharge_floor_soc"]; !exists || v == nil {
				inject["discharge_floor_soc"] = *b.SoCMin
			}
		}
		if len(inject) == 0 {
			continue
		}
		merged := make(map[string]any, len(out[i].Config)+len(inject))
		for k, v := range out[i].Config {
			merged[k] = v
		}
		for k, v := range inject {
			merged[k] = v
		}
		out[i].Config = merged
	}
	return out
}
