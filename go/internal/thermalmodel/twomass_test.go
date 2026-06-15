package thermalmodel

import (
	"math"
	"testing"
	"time"
)

// trueTwoMassStep advances a synthetic 2R2C plant.
func trueTwoMassStep(room, slab, outdoor, heat, dt, aRS, bP, aSR, aRO float64) (nr, ns float64) {
	dSlab := aRS*(room-slab) + bP*heat
	dRoom := aSR*(slab-room) + aRO*(outdoor-room)
	return room + dt*dRoom, slab + dt*dSlab
}

// TestTwoMassRecoversParams drives the model with a synthetic floor-heated
// room and checks both regressions converge to the true coefficients.
func TestTwoMassRecoversParams(t *testing.T) {
	const (
		aRS = 1.0 / 5400.0 // slab↔room
		bP  = 1.5e-7       // slab heating gain
		aSR = 1.0 / 2700.0 // room gains from slab
		aRO = 1.0 / 10800.0
		dt  = 300.0 // 5-min steps
	)
	m := NewTwoMass()
	room, slab, outdoor := 21.0, 24.0, 2.0
	now := time.Now().UnixMilli()
	for k := 0; k < 6000; k++ {
		heat := 0.0
		if k%3 != 0 {
			heat = 1500 + 500*math.Sin(float64(k)/7.0)
		}
		nr, ns := trueTwoMassStep(room, slab, outdoor, heat, dt, aRS, bP, aSR, aRO)
		m.Update(room, nr, slab, ns, outdoor, heat, dt, now)
		room, slab = nr, ns
		now += int64(dt) * 1000
		if k%40 == 0 {
			outdoor = -5 + 10*math.Sin(float64(k)/50.0)
		}
		// keep states in a sane band
		if room > 24 {
			room = 24
		}
		if room < 18 {
			room = 18
		}
		if slab > 30 {
			slab = 30
		}
		if slab < 20 {
			slab = 20
		}
	}
	checks := []struct {
		name      string
		got, want float64
	}{
		{"aRS", m.BetaSlab[0], aRS},
		{"bP", m.BetaSlab[1], bP},
		{"aSR", m.BetaRoom[0], aSR},
		{"aRO", m.BetaRoom[1], aRO},
	}
	for _, c := range checks {
		if rel := math.Abs(c.got-c.want) / c.want; rel > 0.2 {
			t.Errorf("%s: got %.3e want %.3e (rel %.0f%%)", c.name, c.got, c.want, rel*100)
		}
	}
}

// TestTwoMassCoastLongerThanSingleMass is the whole point: with a hot slab,
// the room coasts above target far longer than a single-mass model — which
// ignores the slab's stored heat — would predict.
func TestTwoMassCoastLongerThanSingleMass(t *testing.T) {
	tm := NewTwoMass()
	tm.Samples = WarmupSamples // trust priors fully

	// Room at 21, target 20, but the slab is hot (28°C) — a freshly charged
	// floor. The two-mass coast should be substantial.
	coast2 := tm.CoastHoursToRoomTarget(21.0, 28.0, 20.0, 2.0, 24*time.Hour)

	// Single-mass equivalent with the same room time constant sees only the
	// room air and no slab reservoir.
	sm := NewModel()
	sm.Samples = WarmupSamples
	coast1 := sm.CoastHoursToTarget(21.0, 20.0, 2.0, 24*time.Hour)

	if coast2 <= coast1 {
		t.Errorf("two-mass coast (%.2fh) should exceed single-mass (%.2fh) when the slab is hot", coast2, coast1)
	}
	if coast2 <= 0 {
		t.Error("expected a positive coast time with a hot slab")
	}
}

// TestTwoMassForecastShape sanity-checks the forecast: with no heat and a
// hot slab, the room first holds/rises (slab feeds it) then decays toward
// outdoor.
func TestTwoMassForecastShape(t *testing.T) {
	tm := NewTwoMass()
	tm.Samples = WarmupSamples
	n := 48 // 4h at 5-min steps
	heat := make([]float64, n)
	outdoor := make([]float64, n)
	for i := range outdoor {
		outdoor[i] = 0
	}
	traj := tm.ForecastRoom(20.0, 30.0 /*hot slab*/, heat, outdoor, 300)
	if len(traj) != n {
		t.Fatalf("want %d points, got %d", n, len(traj))
	}
	// With a hot slab the room should not immediately crash; the last point
	// must still be above outdoor.
	if traj[len(traj)-1] <= 0 {
		t.Errorf("room should stay above outdoor over the horizon, ended %.2f", traj[len(traj)-1])
	}
}

// TestTwoMassRejectsGlitch ensures an impossible slab jump is ignored.
func TestTwoMassRejectsGlitch(t *testing.T) {
	tm := NewTwoMass()
	if tm.Update(20, 20.1, 24, 60 /*+36°C in a minute*/, 5, 0, 60, time.Now().UnixMilli()) {
		t.Error("expected glitch step to be rejected")
	}
}
