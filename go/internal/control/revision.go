package control

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
	"sync"
)

// Revision turns "the controllable state changed" into a number.
//
// The app sends the revision a command expected to act against, and the box
// refuses the command when they differ. That check is only worth having if the
// number actually moves: a revision nothing ever increments makes every
// conflict check pass, which is worse than no check at all because it reads
// like one.
//
// It is derived rather than incremented at each mutation site. Control state is
// written from the API, from Home Assistant, from the config reload and from
// main's own loop, all by plain field assignment under the caller's mutex, and
// a counter those places had to remember to bump would be wrong within a
// month. A fingerprint cannot be forgotten.
//
// What counts as controllable is an explicit list below rather than the whole
// struct. Most of State is per-tick working state — last dispatch, slot
// accounting, PI internals — and folding that in would bump the revision every
// second, which would make every command a conflict.
type Revision struct {
	mu   sync.Mutex
	last [sha256.Size]byte
	have bool
	rev  uint64
}

// Observe returns the current revision, bumping it when the controllable state
// differs from the last time it was called.
//
// Monotonic for the life of the process. It restarts at zero across a reboot,
// which is correct: a session cannot span one, and the app resyncs from the
// snapshot it is sent when it reconnects.
func (r *Revision) Observe(s *State) uint64 {
	sum := fingerprint(s)

	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.have {
		r.have = true
		r.last = sum
		r.rev = 1
		return r.rev
	}
	if sum != r.last {
		r.last = sum
		r.rev++
	}
	return r.rev
}

// Current is the revision without observing a change. For readers that must
// not move the counter.
func (r *Revision) Current() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rev
}

// fingerprint hashes the state a user can steer.
//
// Adding a controllable field means adding it here. That is deliberate: a new
// field silently outside the fingerprint is a command conflict that never
// fires, and making the author think about it once is cheaper than finding out
// from a house.
func fingerprint(s *State) [sha256.Size]byte {
	if s == nil {
		return [sha256.Size]byte{}
	}

	h := sha256.New()
	writeString(h, string(s.Mode))
	writeFloat(h, s.GridTargetW)
	writeFloat(h, s.GridToleranceW)
	writeFloat(h, s.PeakLimitW)
	writeFloat(h, s.PeakImportCeilingW)
	writeFloat(h, s.MaxExportW)
	writeFloat(h, s.ManualEVChargingW)
	writeBool(h, s.BatteryCoversEV)

	for _, name := range s.PriorityOrder {
		writeString(h, name)
	}

	// Sorted, because the same weights in a different map iteration order are
	// the same weights, and a revision that moved on iteration order would
	// make every command a conflict at random.
	names := make([]string, 0, len(s.Weights))
	for name := range s.Weights {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeString(h, name)
		writeFloat(h, s.Weights[name])
	}

	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

type byteWriter interface{ Write([]byte) (int, error) }

// Lengths are hashed with the values so that two fields cannot be run together
// into one string that another pair of values would also produce.
func writeString(h byteWriter, v string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(v)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(v))
}

func writeFloat(h byteWriter, v float64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(v))
	_, _ = h.Write(b[:])
}

func writeBool(h byteWriter, v bool) {
	if v {
		_, _ = h.Write([]byte{1})
		return
	}
	_, _ = h.Write([]byte{0})
}
