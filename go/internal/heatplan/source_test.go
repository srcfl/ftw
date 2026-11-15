package heatplan

import (
	"math"
	"testing"
)

func TestDirectElectricIsOneEverywhere(t *testing.T) {
	s := Source{Kind: SourceDirectElectric}
	for _, c := range [][2]float64{{-20, 20}, {0, 65}, {25, 38}} {
		if got := s.COP(c[0], c[1]); got != 1 {
			t.Fatalf("COP(%v, %v) = %v", c[0], c[1], got)
		}
	}
}

// The undeclared pump has to land where real ones do, or every plan built on
// it is wrong by the same factor.
func TestDefaultHeatPumpIsInTheRangeOfRealOnes(t *testing.T) {
	water := Source{Kind: SourceHeatPump}
	air := Source{Kind: SourceHeatPump, SinkApproachK: 18}
	for _, c := range []struct {
		name            string
		src             Source
		outdoor, sink   float64
		atLeast, atMost float64
	}{
		{"air to water, 0 °C, 35 °C flow", water, 0, 35, 3.0, 4.0},
		{"air to water, −15 °C, 45 °C flow", water, -15, 45, 2.0, 2.7},
		{"hot water at 55 °C, 0 °C", water, 0, 55, 2.2, 3.0},
		{"air to air, 0 °C, 21 °C room", air, 0, 21, 3.0, 4.2},
		{"air to air, −15 °C, 21 °C room", air, -15, 21, 2.2, 3.0},
	} {
		if got := c.src.COP(c.outdoor, c.sink); got < c.atLeast || got > c.atMost {
			t.Errorf("%s: COP %.2f, want %.1f–%.1f", c.name, got, c.atLeast, c.atMost)
		}
	}
}

func TestHeatPumpCOPFollowsTheLift(t *testing.T) {
	s := Source{Kind: SourceHeatPump}
	if !(s.COP(5, 35) > s.COP(-5, 35)) {
		t.Fatal("warmer outdoor air must raise the COP")
	}
	if !(s.COP(0, 30) > s.COP(0, 40) && s.COP(0, 40) > s.COP(0, 60)) {
		t.Fatal("a hotter sink must lower the COP")
	}
	poor := Source{Kind: SourceHeatPump, CarnotFraction: 0.1}
	if got := poor.COP(-30, 70); got != 1 {
		t.Fatalf("COP %.2f; a pump never does worse than its backup heater", got)
	}
	if got := s.COP(30, 20); got != defaultMaxCOP {
		t.Fatalf("COP %.2f with no lift, want the cap %v", got, defaultMaxCOP)
	}
	brine := 2.0
	ground := Source{Kind: SourceHeatPump, ColdSideC: &brine, FrostLoss: 0.2}
	if ground.COP(-20, 35) != ground.COP(10, 35) {
		t.Fatal("a ground-source pump must not follow the outdoor air, or frost")
	}
}

func TestDeclaredCurveIsUsedAndScaledToTheSink(t *testing.T) {
	s := Source{Kind: SourceHeatPump, CurveSinkC: 35,
		Curve: []COPPoint{{-15, 2.2}, {-7, 2.8}, {2, 3.6}, {7, 4.6}}}
	for outdoor, want := range map[float64]float64{-20: 2.2, -7: 2.8, -2.5: 3.2, 7: 4.6, 15: 4.6} {
		if got := s.COP(outdoor, 35); math.Abs(got-want) > 1e-9 {
			t.Errorf("COP at %v °C and the curve's own sink = %v, want %v", outdoor, got, want)
		}
	}
	// 20 K hotter costs roughly a quarter to a third at this lift.
	if ratio := s.COP(2, 55) / s.COP(2, 35); ratio < 0.6 || ratio > 0.8 {
		t.Fatalf("COP at 55 °C is %.2f of the COP at 35 °C", ratio)
	}
}

func TestFrostCostsMostJustAboveFreezing(t *testing.T) {
	s := Source{Kind: SourceHeatPump, FrostLoss: 0.15}
	dry := Source{Kind: SourceHeatPump}
	loss := func(outdoor float64) float64 { return 1 - s.COP(outdoor, 35)/dry.COP(outdoor, 35) }
	if math.Abs(loss(1)-0.15) > 1e-9 {
		t.Fatalf("loss at +1 °C = %.3f, want 0.15", loss(1))
	}
	for _, outdoor := range []float64{-15, -7, 6, 12} {
		if loss(outdoor) != 0 {
			t.Errorf("loss at %v °C = %.3f, want none", outdoor, loss(outdoor))
		}
	}
	if !(loss(-3) > 0 && loss(-3) < 0.15 && loss(4) > 0 && loss(4) < 0.15) {
		t.Fatal("the loss must fade on both sides of +1 °C")
	}
}

func TestSupplyCurve(t *testing.T) {
	c := SupplyCurve{ColdOutdoorC: -15, ColdSupplyC: 45, MildOutdoorC: 15, MildSupplyC: 25}
	for outdoor, want := range map[float64]float64{-30: 45, -15: 45, 0: 35, 15: 25, 25: 25} {
		if got := c.At(outdoor); math.Abs(got-want) > 1e-9 {
			t.Errorf("flow at %v °C = %v, want %v", outdoor, got, want)
		}
	}
}

func TestSourceValidation(t *testing.T) {
	hot := 60.0
	for name, s := range map[string]Source{
		"unknown kind":        {Kind: "district"},
		"carnot above one":    {Kind: SourceHeatPump, CarnotFraction: 1.2},
		"cold side too hot":   {Kind: SourceHeatPump, ColdSideC: &hot},
		"cooling curve":       {Kind: SourceHeatPump, Supply: &SupplyCurve{ColdOutdoorC: -15, ColdSupplyC: 25, MildOutdoorC: 15, MildSupplyC: 45}},
		"cop below one":       {Kind: SourceHeatPump, CurveSinkC: 35, Curve: []COPPoint{{0, 0.8}}},
		"unordered curve":     {Kind: SourceHeatPump, CurveSinkC: 35, Curve: []COPPoint{{5, 4}, {0, 3}}},
		"curve with no sink":  {Kind: SourceHeatPump, Curve: []COPPoint{{0, 3}}},
		"frost loss too high": {Kind: SourceHeatPump, FrostLoss: 0.9},
	} {
		if s.validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (Source{Kind: SourceDirectElectric, CarnotFraction: 9}).validate(); err != nil {
		t.Fatalf("direct electric must ignore heat pump fields: %v", err)
	}
}
