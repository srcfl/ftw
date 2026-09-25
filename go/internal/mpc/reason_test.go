package mpc

import (
	"strings"
	"testing"
)

// TestReasonForLabelsBranchOnGridW is the regression test for
// Incident C (Erik, 2026-04-18 22:00) where an 8 kW discharge into
// a 6.6 kW grid export got labelled "cover local load" because the
// label logic only looked at baseline, not at the resulting grid
// direction. Operator couldn't tell a defensive discharge from a
// peak-export arbitrage.
func TestReasonForLabelsBranchOnGridW(t *testing.T) {
	cases := []struct {
		name        string
		loadW, pvW  float64
		priceOre    float64
		meanPrice   float64
		batteryW    float64
		gridW       float64
		wantContain string
	}{
		{
			name:        "heavy discharge + export at peak price → export at peak",
			loadW:       2030, pvW: -640,
			priceOre: 251, meanPrice: 183,
			batteryW: -8000, gridW: -6620,
			wantContain: "export at peak",
		},
		{
			name:        "moderate discharge + small export → export",
			loadW:       2030, pvW: -640,
			priceOre: 150, meanPrice: 183,
			batteryW: -3000, gridW: -500,
			wantContain: "export",
		},
		{
			name:        "discharge covering positive grid import → cover local load",
			loadW:       3480, pvW: -1390,
			priceOre: 166, meanPrice: 180,
			batteryW: -1500, gridW: 590,
			wantContain: "cover local load",
		},
		{
			name:        "discharge with above-mean price but zero grid → peak-shave",
			loadW:       2000, pvW: 0,
			priceOre: 250, meanPrice: 180,
			batteryW: -2000, gridW: 0,
			wantContain: "price above horizon mean",
		},
		{
			name:        "charge + importing at cheap price → cheap-grid charge",
			loadW:       500, pvW: 0,
			priceOre: 50, meanPrice: 180,
			batteryW: 3000, gridW: 3500,
			wantContain: "cheap grid",
		},
		{
			name:        "idle exporting PV",
			loadW:       500, pvW: -2000,
			priceOre: 100, meanPrice: 180,
			batteryW: 0, gridW: -1500,
			wantContain: "export PV",
		},
		{
			name:        "idle importing to cover load",
			loadW:       3000, pvW: -500,
			priceOre: 100, meanPrice: 180,
			batteryW: 0, gridW: 2500,
			wantContain: "import to cover",
		},
		// Codex P2 on PR #119 — the "absorb PV surplus" branch used
		// !gridExports which broke both edges.
		{
			// Partial PV absorption: battery charges, takes some of
			// the surplus, rest still exports to grid. Operator is
			// seeing the battery act as a solar sink, so the label
			// should reflect that — not generic "charge".
			name:        "partial PV absorption — battery charging, grid still exporting",
			loadW:       1000, pvW: -4000,
			priceOre: 100, meanPrice: 180,
			batteryW: 2000, gridW: -1000, // baseline=-3000, grid exports 1kW residual
			wantContain: "absorb PV surplus",
		},
		{
			// Over-charging: battery charges harder than PV provides,
			// dragging the grid into actual import. Should NOT be
			// labelled PV absorption — it's grid-charging.
			name:        "over-charging — battery takes more than PV, grid imports",
			loadW:       1000, pvW: -1500,
			priceOre: 100, meanPrice: 180,
			batteryW: 3000, gridW: 2500, // baseline=-500, battery > PV → import 2.5kW
			wantContain: "cheap grid", // 100 < 180*0.9 → priceBelow branch
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Slot{LoadW: tc.loadW, PVW: tc.pvW, PriceOre: tc.priceOre}
			got := reasonFor(s, tc.batteryW, tc.gridW, tc.meanPrice)
			if !strings.Contains(got, tc.wantContain) {
				t.Errorf("reason = %q, want substring %q", got, tc.wantContain)
			}
		})
	}
}
