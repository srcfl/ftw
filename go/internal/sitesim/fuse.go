package sitesim

import (
	"fmt"

	"github.com/srcfl/device-simulator/fuse"
)

// Fuse is a gG main fuse (IEC 60269) on each phase, as srcfl/device-simulator's
// fuse package models it: the thermal model the site rig judges Core by, so
// both simulators share one fuse. A phase starts cold in a 25 °C cabinet;
// Preheat starts it warm. Damage and Blown mirror each phase after a step.
type Fuse struct {
	RatingA float64
	Phase   [3]fuse.Phase
	Damage  [3]float64
	Blown   [3]bool
}

var gG, _ = fuse.Lookup(fuse.IecGg)

// AnyBlown reports whether any phase has blown.
func (f *Fuse) AnyBlown() bool { return f.Blown[0] || f.Blown[1] || f.Blown[2] }

// Preheat puts each phase in the steady state of amps held for hours, for a
// scenario that starts with the house near its limit.
func (f *Fuse) Preheat(amps [3]float64) error {
	for i, a := range amps {
		if err := gG.Preheat(&f.Phase[i], a, f.RatingA); err != nil {
			return fmt.Errorf("L%d: %w", i+1, err)
		}
		f.Damage[i] = f.Phase[i].Damage
	}
	return nil
}

// step panics on a step the model refuses, such as a rating it does not
// model: a scenario that cannot be judged is a broken test.
func (f *Fuse) step(amps [3]float64, dt float64) {
	for i, a := range amps {
		if st := gG.Step(&f.Phase[i], a, f.RatingA, dt); st.Invalid != "" {
			panic(fmt.Sprintf("sitesim: L%d fuse: %s", i+1, st.Invalid))
		}
		f.Damage[i], f.Blown[i] = f.Phase[i].Damage, f.Phase[i].Blown
	}
}
