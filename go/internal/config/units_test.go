package config

import "testing"

func TestNormalizeUnitsFoldsLegacyKWpAndPercent(t *testing.T) {
	tilt, az := 27.0, 150.0
	c := &Config{
		Planner: &Planner{SoCMinPct: 10, SoCMaxPct: 90},
		Weather: &Weather{
			PVRatedW: 18960,
			PVArrays: []PVArray{
				{Name: "east", KWp: 12960, TiltDeg: &tilt, AzimuthDeg: &az},
				{Name: "south", KWp: 6, TiltDeg: &tilt, AzimuthDeg: &az},
			},
		},
		CalDAV:   &CalDAV{Enabled: true, EVDefaultTargetSoCPct: 80},
		Site:     Site{PVSurplusAbsorbSoCCapPct: 88},
		Vehicles: []Vehicle{{ID: "leaf", TargetSoCPct: 80}},
	}
	c.NormalizeUnits()
	if c.Planner.SoCMin != 0.10 || c.Planner.SoCMax != 0.90 {
		t.Fatalf("planner SoC = %v..%v, want 0.10..0.90", c.Planner.SoCMin, c.Planner.SoCMax)
	}
	if c.Weather.PVArrays[0].RatedW != 12960 || c.Weather.PVArrays[0].KWp != 0 {
		t.Fatalf("pasted watts-as-kwp: %+v", c.Weather.PVArrays[0])
	}
	if c.Weather.PVArrays[1].RatedW != 6000 {
		t.Fatalf("6 kWp → %v W, want 6000", c.Weather.PVArrays[1].RatedW)
	}
	if c.CalDAV.EVDefaultTargetSoC != 0.80 {
		t.Fatalf("caldav default SoC = %v, want 0.80", c.CalDAV.EVDefaultTargetSoC)
	}
	if c.Site.PVSurplusAbsorbSoCCap != 0.88 {
		t.Fatalf("absorb cap = %v, want 0.88", c.Site.PVSurplusAbsorbSoCCap)
	}
	if c.Vehicles[0].TargetSoC != 0.80 || c.Vehicles[0].TargetSoCPct != 0 {
		t.Fatalf("vehicle target SoC = %+v, want 0.80 with legacy pct cleared", c.Vehicles[0])
	}
}
