// Package drivercontrol owns what is held on a driver: one bounded value at a
// time, sent and released under one per-driver lock, and handed back to the
// device's own default when it ends.
//
// It moved here from the API package unchanged so that more than one caller
// can share it. An operator's hold and, later, a planned value must go
// through the same lock and the same expiry, or each would expire into a
// default that undoes the other (docs/adr/0012).
//
// Holds are process-lifetime only, deliberately: a restart should leave no
// device held by a setting nobody remembers making.
package drivercontrol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/drivers"
)

// DefaultTimeout bounds a default sent without a caller deadline, and a
// command sent while the per-driver lock is held.
const DefaultTimeout = 10 * time.Second

// Registry is the part of drivers.Registry holds need.
type Registry interface {
	SendWithGeneration(ctx context.Context, name string, payload []byte) (uint64, error)
	ControlStatus(name string) (drivers.DriverControlStatus, bool)
	SendDefault(ctx context.Context, name string) error
}

// Hold is one active setting. The timer is what releases it; the fields are
// what the UI shows meanwhile.
type Hold struct {
	Control    string `json:"control"`
	Value      any    `json:"value,omitempty"`
	ExpiresAt  int64  `json:"expires_at_ms"`
	Generation uint64 `json:"-"`

	armedAt time.Time
	timer   *time.Timer
}

// State serializes command dispatch, default dispatch and hold transitions
// for one driver. It is opaque outside this package: callers go through
// Holds' methods, which take and release its lock themselves.
type State struct {
	mu   sync.Mutex
	hold *Hold
}

// Holds is every driver's State.
type Holds struct {
	registry func() Registry

	mu     sync.Mutex
	states map[string]*State

	// Test seams, nil in production and set only through Seams. Guarded by
	// mu.
	beforeStateLock        func()
	beforeDefaultStateLock func()
}

// New returns empty holds. registry is read at each use, so a caller whose
// registry is absent gets the same answers it did before this package
// existed.
func New(registry func() Registry) *Holds {
	return &Holds{registry: registry, states: make(map[string]*State)}
}

func (h *Holds) reg() Registry {
	if h.registry == nil {
		return nil
	}
	return h.registry()
}

// Send dispatches body to the driver and holds controlID at value for d.
// The send and the hold transition happen under one per-driver lock, and the
// device call has a deadline because hold expiry waits on that lock. On error
// any existing hold is cleared and nothing is armed.
//
// done runs with the outcome while the lock is still held, so a caller that
// reads registry status to explain an error sees it before any lifecycle
// change waiting on this driver can proceed.
func (h *Holds) Send(ctx context.Context, name, controlID string, value any, body []byte, d time.Duration, done func(Hold, error)) {
	reg := h.reg()
	if reg == nil {
		done(Hold{}, errors.New("driver registry not available"))
		return
	}
	// Keep command dispatch and hold transitions under one per-driver lock.
	// Registry.SendWithGeneration selects the concrete running instance and
	// returns its generation, so expiry can never pair a new command with an
	// old status snapshot. The state lookup and lock are also one lifecycle
	// transaction: clearState takes the same locks in the same order, so it
	// cannot remove the map entry between these operations.
	state := h.lock(name)
	defer state.mu.Unlock()
	sendCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	generation, err := reg.SendWithGeneration(sendCtx, name, body)
	if err != nil {
		h.clearLocked(state)
		done(Hold{}, err)
		return
	}
	hold := h.armLocked(name, state, controlID, value, generation, d)
	out := *hold
	out.timer = nil
	done(out, nil)
}

func (h *Holds) stateLocked(name string) *State {
	if h.states == nil {
		h.states = make(map[string]*State)
	}
	state := h.states[name]
	if state == nil {
		state = &State{}
		h.states[name] = state
	}
	return state
}

// lock serializes a state map lookup with acquisition of that driver's
// mutex. Lifecycle invalidation uses h.mu -> state.mu as well, so a request
// can either own the state before removal starts or see a removed state
// after removal finishes; it cannot keep a pointer that was deleted in
// between.
func (h *Holds) lock(name string) *State {
	h.mu.Lock()
	state := h.stateLocked(name)
	if h.beforeStateLock != nil {
		h.beforeStateLock()
	}
	state.mu.Lock()
	h.mu.Unlock()
	return state
}

// tryLockExisting locks a state that already exists without creating one
// for a driver that may have been removed concurrently, and without waiting:
// locked is false when another request holds the state. Lifecycle
// invalidation uses the same h.mu -> state.mu order.
func (h *Holds) tryLockExisting(name string) (state *State, locked bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state = h.states[name]
	if state == nil {
		return nil, false
	}
	return state, state.mu.TryLock()
}

// armLocked replaces any existing hold for the driver; the caller holds
// state's lock. One driver holds one control at a time: two overlapping
// holds on the same device would each expire into a default that undoes the
// other.
func (h *Holds) armLocked(name string, state *State, control string, value any, generation uint64, d time.Duration) *Hold {
	h.clearLocked(state)
	now := time.Now()
	hold := &Hold{
		Control:    control,
		Value:      value,
		ExpiresAt:  now.Add(d).UnixMilli(),
		Generation: generation,
		armedAt:    now,
	}
	hold.timer = time.AfterFunc(d, func() {
		if err := h.expire(name, state, hold); err != nil {
			slog.Error("control hold expiry default failed; registry recovery gate remains active", "driver", name, "err", err)
		}
	})
	state.hold = hold
	return hold
}

func (h *Holds) clearLocked(state *State) {
	if state.hold != nil && state.hold.timer != nil {
		state.hold.timer.Stop()
	}
	state.hold = nil
}

// expire hands the device back to itself. It checks identity first: a hold
// that was replaced or released already had its timer stopped, but a timer
// that had begun firing cannot be stopped, and defaulting a driver that an
// operator has just set again is the one wrong answer here.
func (h *Holds) expire(name string, state *State, fired *Hold) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.hold != fired {
		return nil
	}
	reg := h.reg()
	if reg == nil {
		h.clearLocked(state)
		return errors.New("driver registry not available")
	}
	status, ok := reg.ControlStatus(name)
	if !ok || status.Generation != fired.Generation {
		// A stopped generation must never default a replacement instance.
		h.clearLocked(state)
		return nil
	}
	h.clearLocked(state)
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	return h.sendDefaultLocked(ctx, name)
}

// SendDefault is the shared safety path for watchdogs and release. It clears
// the hold while holding the same per-driver lock used by command dispatch,
// then sends the driver's own default with a bounded context. A caller
// without a deadline gets the 10-second safety deadline.
//
// The control tick calls this, so it never waits for a command in flight.
// When one holds the lock, the default goes first: SendDefault cancels that
// command and refuses new ones until the default is confirmed. The hold is
// cleared once the lock is free, but only a hold armed before the default
// returned. A later one belongs to a command sent after the default and
// keeps its own expiry.
func (h *Holds) SendDefault(ctx context.Context, name string) error {
	reg := h.reg()
	if reg == nil {
		return errors.New("driver registry not available")
	}
	if _, ok := reg.ControlStatus(name); !ok {
		return fmt.Errorf("driver %q not found", name)
	}
	h.mu.Lock()
	before := h.beforeDefaultStateLock
	h.mu.Unlock()
	if before != nil {
		before()
	}
	state, locked := h.tryLockExisting(name)
	if locked {
		defer state.mu.Unlock()
		h.clearLocked(state)
		return h.sendDefaultLocked(ctx, name)
	}
	err := h.sendDefaultLocked(ctx, name)
	if state != nil {
		defaulted := time.Now()
		go func() {
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.hold != nil && !state.hold.armedAt.After(defaulted) {
				h.clearLocked(state)
			}
		}()
	}
	return err
}

func (h *Holds) sendDefaultLocked(ctx context.Context, name string) error {
	reg := h.reg()
	if reg == nil {
		return errors.New("driver registry not available")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	return reg.SendDefault(ctx, name)
}

// Active returns a copy of the driver's hold, or nil. A hold bound to a
// generation that is no longer running is cleared, not returned.
//
// It takes the per-driver lock, so it waits for a command in flight, up to
// DefaultTimeout. That suits a request. The control tick must use TryActive.
func (h *Holds) Active(name string) *Hold {
	state := h.peek(name)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return h.activeLocked(name, state)
}

// TryActive is Active for a caller that must not wait, as the control tick
// must not. known is false when a command or a default holds the driver's
// lock: the hold is then changing, and the caller should treat the driver as
// held and look again on its next tick rather than send anything.
func (h *Holds) TryActive(name string) (hold *Hold, known bool) {
	state, locked := h.tryLockExisting(name)
	if state == nil {
		return nil, true
	}
	if !locked {
		return nil, false
	}
	defer state.mu.Unlock()
	return h.activeLocked(name, state), true
}

func (h *Holds) activeLocked(name string, state *State) *Hold {
	if state.hold == nil {
		return nil
	}
	reg := h.reg()
	if reg == nil {
		h.clearLocked(state)
		return nil
	}
	status, ok := reg.ControlStatus(name)
	if !ok || status.Generation != state.hold.Generation {
		h.clearLocked(state)
		return nil
	}
	hold := *state.hold
	hold.timer = nil
	return &hold
}

// peek returns the driver's state without creating one.
func (h *Holds) peek(name string) *State {
	h.mu.Lock()
	state := h.states[name]
	h.mu.Unlock()
	return state
}

// Clear is called by the registry before a driver generation is removed. It
// stops the old timer and drops the map entry so a re-added driver cannot
// inherit a hold from the previous instance.
func (h *Holds) Clear(name string) {
	h.clearState(name, nil)
}

func (h *Holds) clearState(name string, expected *State) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.states[name]
	if expected != nil && state != expected {
		return
	}
	if state == nil {
		return
	}
	state.mu.Lock()
	h.clearLocked(state)
	delete(h.states, name)
	state.mu.Unlock()
}
