package heatplan

import (
	"math"
	"testing"
)

// The rule one household runs today: a bathroom floor that follows cheap
// days up, and room air that only follows dear days down. Prices in öre.
var (
	floorComfort = PriceComfort{CheapPrice: 150, NormalPrice: 260, DearPrice: 400, RaiseK: 2.5}
	airComfort   = PriceComfort{CheapPrice: 150, NormalPrice: 260, DearPrice: 400, LowerK: 1, StepK: 0.5}
)

func TestPriceComfortMovesGraduallyWithTheDaysPrice(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    PriceComfort
		mean float64
		want float64
	}{
		{"floor, cheaper than cheap", floorComfort, 90, 2.5},
		{"floor, at cheap", floorComfort, 150, 2.5},
		{"floor, halfway", floorComfort, 205, 1.25},
		{"floor, normal", floorComfort, 260, 0},
		{"floor never follows a dear day down", floorComfort, 480, 0},
		{"air never follows a cheap day up", airComfort, 90, 0},
		{"air, normal", airComfort, 260, 0},
		{"air, a little dear rounds to no step", airComfort, 290, 0},
		{"air, a quarter of the way is half a step up to 0.5", airComfort, 295, -0.5},
		{"air, most of the way", airComfort, 343, -0.5},
		{"air, three quarters rounds to the whole", airComfort, 365, -1},
		{"air, dearer than dear", airComfort, 480, -1},
	} {
		if got := tc.c.ShiftK(tc.mean); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: shift at %v = %v K, want %v", tc.name, tc.mean, got, tc.want)
		}
	}
}

// Nothing is built in: a rule nobody set, a rule that contradicts itself and
// a mean that is not a number all leave the owner's target alone.
func TestPriceComfortWithoutARuleChangesNothing(t *testing.T) {
	z := room(day, 40, 8, 0)
	for name, c := range map[string]PriceComfort{
		"zero value":          {},
		"cheap above normal":  {CheapPrice: 300, NormalPrice: 260, DearPrice: 400, RaiseK: 2},
		"dear below normal":   {CheapPrice: 150, NormalPrice: 260, DearPrice: 200, LowerK: 1},
		"equal prices":        {CheapPrice: 260, NormalPrice: 260, DearPrice: 260, RaiseK: 2, LowerK: 1},
		"negative shift":      {CheapPrice: 150, NormalPrice: 260, DearPrice: 400, RaiseK: -1, LowerK: 1},
		"not a number":        {CheapPrice: math.NaN(), NormalPrice: 260, DearPrice: 400, RaiseK: 2},
		"a step out of range": {CheapPrice: 150, NormalPrice: 260, DearPrice: 400, LowerK: 1, StepK: 9},
	} {
		for _, mean := range []float64{50, 260, 900} {
			if got := c.Apply(z, mean); &got.TargetC[0] != &z.TargetC[0] {
				t.Errorf("%s: target moved at a mean of %v", name, mean)
			}
		}
		if name != "zero value" && c.Validate() == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
	if (PriceComfort{}).Validate() != nil || (PriceComfort{}).Enabled() {
		t.Error("the zero value must be valid and off")
	}
	if got := floorComfort.Apply(z, math.NaN()); &got.TargetC[0] != &z.TargetC[0] {
		t.Error("a mean that is not a number moved the target")
	}
	if floorComfort.Validate() != nil || airComfort.Validate() != nil {
		t.Error("the household's rules must be valid")
	}
}

// The band is the owner's hard limit. Comfort moves the target inside it,
// and leaves the zone it was given untouched.
func TestPriceComfortStaysInsideTheBand(t *testing.T) {
	z := room(day, 40, 8, 0) // target 21, band 20 to 22
	up := floorComfort.Apply(z, 100)
	down := PriceComfort{NormalPrice: 260, DearPrice: 400, LowerK: 3}.Apply(z, 500)
	for i := range z.TargetC {
		if up.TargetC[i] != 22 || down.TargetC[i] != 20 {
			t.Fatalf("slot %d: target %v up and %v down, want the band's 22 and 20", i, up.TargetC[i], down.TargetC[i])
		}
		if z.TargetC[i] != 21 {
			t.Fatalf("slot %d: the caller's zone was changed to %v", i, z.TargetC[i])
		}
	}
}

// The baseline is the device left alone at the target, so on a dear day it
// is the cooler target the owner said that day is worth: the plan is not
// credited with the comfort rule's saving.
func TestBaselineFollowsTheDaysComfort(t *testing.T) {
	price := flat(day, 450)
	r := radiatorHouse(day, price)
	mean, ok := DayMean(price, slotH, 22)
	if !ok || mean != 450 {
		t.Fatalf("DayMean = %v, %v", mean, ok)
	}
	usual := propose(t, r)
	r.Zone = airComfort.Apply(r.Zone, mean)
	if r.Zone.TargetC[0] != 20 {
		t.Fatalf("target on a dear day = %v, want 20", r.Zone.TargetC[0])
	}
	dear := propose(t, r)
	if !(dear.BaselineCost < usual.BaselineCost) {
		t.Errorf("baseline on a dear day = %v, want below the usual %v", dear.BaselineCost, usual.BaselineCost)
	}
	if s := dear.Saving(); s > 0.01*dear.BaselineCost {
		t.Errorf("a flat price left %v to save against a baseline of %v", s, dear.BaselineCost)
	}
}

func TestDayMeanNeedsMostOfTheDay(t *testing.T) {
	if _, ok := DayMean(flat(80, 100), slotH, 22); ok {
		t.Error("20 hours passed for a day")
	}
	if m, ok := DayMean(flat(92, 100), slotH, 22); !ok || m != 100 {
		t.Errorf("23 hours (a day the clocks go forward) = %v, %v", m, ok)
	}
	// A day the clocks go back has 25 hours; its mean is still the mean of
	// every slot in it, not of 24 hours.
	long := append(flat(96, 100), flat(4, 200)...)
	if m, ok := DayMean(long, slotH, 22); !ok || math.Abs(m-104) > 1e-9 {
		t.Errorf("25 hours (a day the clocks go back) = %v, %v; want 104", m, ok)
	}
	if _, ok := DayMean([]float64{100, math.NaN()}, 12, 22); ok {
		t.Error("a price that is not a number passed")
	}
	if _, ok := DayMean(nil, slotH, 0); ok {
		t.Error("no prices passed")
	}
}

func saves(v float64) Plan { return Plan{Cost: 1000 - v, BaselineCost: 1000} }

// What a plan seems to save by leaving its band is comfort it did not
// deliver, and a plan that costs more than leaving the device alone saves
// nothing rather than less than nothing.
func TestSavingCountsOnlyWhatAFeasiblePlanSaves(t *testing.T) {
	out := saves(900)
	out.BandExcursionKh = 0.5
	worse := Plan{Cost: 1100, BaselineCost: 1000}
	broken := Plan{Cost: math.NaN(), BaselineCost: 1000}
	if out.Saving() != 0 || worse.Saving() != 0 || broken.Saving() != 0 || saves(300).Saving() != 300 {
		t.Errorf("savings = %v, %v, %v, %v; want 0, 0, 0, 300", out.Saving(), worse.Saving(), broken.Saving(), saves(300).Saving())
	}
}

// A step never takes the target past what the owner allowed: 2.5 K in
// whole steps is 2 K, and a step larger than the limit moves nothing.
func TestPriceComfortStepNeverPassesTheOwnersLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    PriceComfort
		mean float64
		want float64
	}{
		{"2.5 K up in 1 K steps", PriceComfort{CheapPrice: 150, NormalPrice: 260, RaiseK: 2.5, StepK: 1}, 100, 2},
		{"1 K up in 2 K steps", PriceComfort{CheapPrice: 150, NormalPrice: 260, RaiseK: 1, StepK: 2}, 100, 0},
		{"1.5 K down in 1 K steps", PriceComfort{NormalPrice: 260, DearPrice: 400, LowerK: 1.5, StepK: 1}, 500, -1},
		{"0.3 K up in 0.1 K steps", PriceComfort{CheapPrice: 150, NormalPrice: 260, RaiseK: 0.3, StepK: 0.1}, 100, 0.3},
		{"a step that fits is kept", PriceComfort{CheapPrice: 150, NormalPrice: 260, RaiseK: 2, StepK: 0.5}, 100, 2},
	} {
		got := tc.c.ShiftK(tc.mean)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: shift = %v K, want %v", tc.name, got, tc.want)
		}
		if got > tc.c.RaiseK+1e-9 || got < -tc.c.LowerK-1e-9 {
			t.Errorf("%s: shift %v K is past the owner's limit", tc.name, got)
		}
	}
}
