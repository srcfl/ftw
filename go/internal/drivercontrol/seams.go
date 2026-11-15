package drivercontrol

import (
	"testing"
	"time"
)

// Seams is what tests need to stand in for a request in flight, a timer
// that fired, or a lifecycle change between two steps. It reaches under the
// per-driver lock, which no production caller may do: a planned-value
// caller that held that lock itself could keep an operator's command, or
// the tick's safety default, waiting on it.
//
// The API package's race tests need it as well as this package's own, so it
// cannot live in a _test file. Seams panics outside a test binary instead.
type Seams struct{ h *Holds }

// Seams returns the test access. It panics outside a test binary.
func (h *Holds) Seams() Seams {
	if !testing.Testing() {
		panic("drivercontrol: Seams is for tests only")
	}
	return Seams{h}
}

// State returns the driver's state, creating it.
func (s Seams) State(name string) *State {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return s.h.stateLocked(name)
}

// Peek returns the driver's state without creating one.
func (s Seams) Peek(name string) *State { return s.h.peek(name) }

// Len is how many drivers have state.
func (s Seams) Len() int {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return len(s.h.states)
}

// Lock takes the per-driver lock, as a command in flight does.
func (s Seams) Lock(st *State) { st.mu.Lock() }

// Unlock releases the per-driver lock.
func (s Seams) Unlock(st *State) { st.mu.Unlock() }

// CurrentHold returns the held setting itself, not a copy, or nil. Its
// identity is what Expire compares.
func (s Seams) CurrentHold(st *State) *Hold {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.hold
}

// ArmLocked arms a hold as Send does after its command; the caller holds
// st's lock.
func (s Seams) ArmLocked(name string, st *State, control string, value any, generation uint64, d time.Duration) *Hold {
	return s.h.armLocked(name, st, control, value, generation, d)
}

// Expire runs what a hold's timer runs.
func (s Seams) Expire(name string, st *State, fired *Hold) error {
	return s.h.expire(name, st, fired)
}

// BeforeStateLock sets a function Send runs after the state lookup, while
// the map lock is held and before the per-driver lock is taken. nil clears
// it.
func (s Seams) BeforeStateLock(fn func()) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.h.beforeStateLock = fn
}

// BeforeDefaultStateLock sets a function SendDefault runs after it has
// found the driver and before it looks for the driver's state. nil clears
// it.
func (s Seams) BeforeDefaultStateLock(fn func()) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.h.beforeDefaultStateLock = fn
}
