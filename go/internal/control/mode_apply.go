package control

import "fmt"

// ApplyMode is where a site mode change happens.
//
// A mode change is three things, not one, and getting two of them without the
// third has already cost a night's battery: the mode itself, dropping any
// active manual hold so the new mode takes effect on the next dispatch tick,
// and resetting the PI integrator. The integral accumulated under the previous
// mode's error signal means nothing to the new one — leaving it in place
// pinned the output in the wrong direction across a live evening switch and
// discharged the fleet to 7 % overnight.
//
// Every door into a mode change goes through here so no future one can arrive
// with two of the three. Callers hold the outer ctrlMu.
func (s *State) ApplyMode(m Mode) error {
	if !IsValidMode(m) {
		return fmt.Errorf("control: unknown mode %q", m)
	}

	s.Mode = m
	s.ClearBatteryManualHold()
	if s.PI != nil {
		s.PI.Reset()
	}
	return nil
}
