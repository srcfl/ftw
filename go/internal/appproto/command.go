package appproto

import (
	"sort"
	"sync"
)

// CmdRetentionMs is how long a command id is remembered.
//
// Twenty-four hours. Long enough that a phone which lost its connection
// mid-command and retried the next morning gets the original outcome back
// instead of acting a second time; short enough that the table stays small.
// Measured in box uptime like everything else here.
const CmdRetentionMs int64 = 24 * 60 * 60 * 1000

// LeaseMs is how long a dispatcher lease stays valid.
const LeaseMs int64 = 15 * 60 * 1000

// opSpec is what an operation needs before it may run.
type opSpec struct {
	// scope the grant must carry.
	scope string
	// dispatchWrite marks operations that move energy. Those are refused
	// while the box's own dispatch gate is blocked — the safety invariant
	// is that stale meter data stops dispatch, and an operation that would
	// dispatch must not slip past it through a different door.
	//
	// Setting the site mode is deliberately not one of these. The mode is
	// state the box holds; the dispatch gate already refuses to act on it
	// until the meter is back, and refusing the mode change too would leave
	// a user unable to pick a safer strategy precisely when a meter is sick.
	dispatchWrite bool
}

// defaultOps is every operation this box will accept. A handler holds its own
// copy so nothing rewrites a package-wide table underneath a live session.
func defaultOps() map[string]opSpec {
	return map[string]opSpec{
		OpSetMode: {scope: ScopeModeWrite, dispatchWrite: false},
	}
}

// cmdRecord is one remembered outcome.
type cmdRecord struct {
	ack CmdAck
	// result is nil until the driver has read the value back. A retry
	// arriving in that window gets the ack again and keeps waiting, which
	// is the truth: the box has the intent and the hardware has not
	// confirmed.
	result   *CmdResult
	storedAt int64
}

// cmdLog is the idempotency table.
type cmdLog struct {
	mu      sync.Mutex
	entries map[string]*cmdRecord
}

func newCmdLog() *cmdLog {
	return &cmdLog{entries: make(map[string]*cmdRecord)}
}

// lookup returns a remembered outcome, if the box has seen this id before.
func (l *cmdLog) lookup(cmdID string) (*cmdRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.entries[cmdID]
	if !ok {
		return nil, false
	}
	copied := *rec
	return &copied, true
}

// remember stores an ack and prunes anything past the retention window.
func (l *cmdLog) remember(cmdID string, ack CmdAck, uptimeMs int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[cmdID] = &cmdRecord{ack: ack, storedAt: uptimeMs}
	for id, rec := range l.entries {
		if uptimeMs-rec.storedAt > CmdRetentionMs {
			delete(l.entries, id)
		}
	}
}

// settle attaches the outcome to a remembered command.
func (l *cmdLog) settle(cmdID string, res CmdResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rec, ok := l.entries[cmdID]; ok {
		copied := res
		rec.result = &copied
	}
}

// size is for tests: the table must not grow without bound.
func (l *cmdLog) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// guardsHold re-evaluates every guard against state as it is now.
//
// Now, not as it was when the user pressed the button. The comparison uses the
// same integers the telemetry stream sends, so a guard written against what
// the app displayed means what the person who wrote it thought it meant.
//
// An unknown field id fails. A guard nobody can evaluate is not a guard that
// passed.
func guardsHold(guards []Guard, fields map[string]int64) (bool, Guard) {
	for _, g := range guards {
		v, ok := fields[fidKey(g.Fid)]
		if !ok {
			return false, g
		}
		fv := float64(v)
		pass := false
		switch g.Op {
		case GuardLT:
			pass = fv < g.Value
		case GuardLTE:
			pass = fv <= g.Value
		case GuardGT:
			pass = fv > g.Value
		case GuardGTE:
			pass = fv >= g.Value
		default:
			// An operator this box does not know cannot be checked, and an
			// unchecked precondition is the one thing a guard must never be.
			pass = false
		}
		if !pass {
			return false, g
		}
	}
	return true, Guard{}
}

// sortedGuardFids is only for error args, so the app can name the guard that
// failed without the box writing a sentence.
func sortedGuardFids(guards []Guard) []int64 {
	out := make([]int64, 0, len(guards))
	for _, g := range guards {
		out = append(out, int64(g.Fid))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
