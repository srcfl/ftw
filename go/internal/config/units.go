package config

import "github.com/srcfl/ftw/go/internal/units"

// NormalizeUnits folds legacy kilo/percent YAML into SI fields.
// Call from Validate so file load and API posts share one door.
// After this runs, core fields are watts and 0–1 fractions; legacy
// keys are cleared so the next write uses the canonical names.
func (c *Config) NormalizeUnits() {
	if c == nil {
		return
	}
	if c.Weather != nil {
		for i := range c.Weather.PVArrays {
			c.Weather.PVArrays[i].normalizeRatedW()
		}
	}
	if c.Planner != nil {
		p := c.Planner
		p.SoCMin = pickFraction(p.SoCMin, p.SoCMinPct)
		p.SoCMax = pickFraction(p.SoCMax, p.SoCMaxPct)
		p.SoCMinPct = 0
		p.SoCMaxPct = 0
	}
	if c.CalDAV != nil {
		c.CalDAV.EVDefaultTargetSoC = pickFraction(c.CalDAV.EVDefaultTargetSoC, c.CalDAV.EVDefaultTargetSoCPct)
		c.CalDAV.EVDefaultTargetSoCPct = 0
	}
	if c.V2X != nil {
		c.V2X.MinReserveSoC = pickFraction(c.V2X.MinReserveSoC, c.V2X.MinReserveSoCPct)
		c.V2X.DepartureTargetSoC = pickFraction(c.V2X.DepartureTargetSoC, c.V2X.DepartureTargetSoCPct)
		c.V2X.MinReserveSoCPct = 0
		c.V2X.DepartureTargetSoCPct = 0
	}
	for i := range c.Loadpoints {
		lp := &c.Loadpoints[i]
		lp.PluginSoC = pickFraction(lp.PluginSoC, lp.PluginSoCPct)
		lp.PluginSoCPct = 0
	}
	for i := range c.Vehicles {
		v := &c.Vehicles[i]
		v.TargetSoC = pickFraction(v.TargetSoC, v.TargetSoCPct)
		v.TargetSoCPct = 0
	}
	c.Site.PVSurplusAbsorbSoCCap = pickFraction(c.Site.PVSurplusAbsorbSoCCap, c.Site.PVSurplusAbsorbSoCCapPct)
	c.Site.PVSurplusAbsorbSoCCapPct = 0
}

func pickFraction(canonical, legacyPercent float64) float64 {
	return units.DecodeJSONFraction(canonical, legacyPercent)
}

func (a *PVArray) normalizeRatedW() {
	if a.RatedW > 0 {
		a.KWp = 0
		return
	}
	if a.KWp > 0 {
		a.RatedW = units.RatedWattsFromLegacyKWp(a.KWp)
		a.KWp = 0
	}
}

// RatedWatts is the array nameplate in W after NormalizeUnits.
func (a PVArray) RatedWatts() float64 {
	if a.RatedW > 0 {
		return a.RatedW
	}
	return units.RatedWattsFromLegacyKWp(a.KWp)
}
