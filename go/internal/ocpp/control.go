package ocpp

// Control for OCPP chargers.
//
// Command has the same shape as drivers.Registry.Send, so the loadpoint
// controller dispatches to an OCPP charger without knowing it is not a Lua
// driver. The command vocabulary is the one every EV driver already implements
// — ev_set_current, ev_pause, ev_start, ev_resume — so nothing upstream of here
// needs a special case.
//
// Everything is expressed as a current limit rather than a remote start/stop.
// That is deliberate: RemoteStopTransaction is unreliable in the field on
// Charge Amps hardware, where units acknowledge the stop and return to charging
// on their own. A charging profile of 0 A is honoured consistently, so pausing
// is "allow zero amps" rather than "end the transaction", and resuming raises
// the limit again. It also leaves the transaction open, so the session meter
// keeps accumulating across a pause instead of being split in two.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

const (
	// IEC 61851 does not allow a duty cycle below 6 A. Charging is either off
	// or at least this much; anything between is refused by the vehicle or
	// leaves it drawing an unpredictable current.
	minChargeAmps = 6.0

	// Fallback electrical assumptions, used only when the command omits them.
	// A command from the loadpoint controller carries the real site values.
	defaultVoltage = 230.0
	defaultPhases  = 3

	// Ceiling applied when a command asks to resume without saying how fast
	// and we have never set a limit for this charger.
	defaultMaxAmps = 16.0

	// How long to wait for a charger to confirm a profile before giving up.
	// Control runs on a tick; a command that has not landed by now is better
	// reported as failed than left blocking the loop.
	commandTimeout = 10 * time.Second

	// Charge-point-wide default profile. Connector 0 means "every connector",
	// which avoids depending on per-connector ids — those are unreliable on
	// dual-socket units such as the Charge Amps Aura.
	allConnectors = 0

	// A single, stable profile id and stack level means each new limit
	// replaces the previous one instead of stacking on top of it.
	ftwProfileID   = 1
	ftwStackLevel  = 0
	scheduleStartS = 0
)

// command is the JSON payload the loadpoint controller sends to EV drivers.
// Only the fields that affect a current limit are read here.
type command struct {
	Action          string  `json:"action"`
	PowerW          float64 `json:"power_w"`
	Voltage         float64 `json:"voltage"`
	SitePhases      int     `json:"site_phases"`
	MaxAmpsPerPhase float64 `json:"max_amps_per_phase"`
	PhaseMode       string  `json:"phase_mode"`
}

// ErrNotConnected is returned when a command targets a charge point that is not
// currently connected. Callers use it to tell "wrong name" apart from "charger
// is offline right now".
var ErrNotConnected = errors.New("ocpp: charger not connected")

// Command applies an EV control command to a connected charge point. The
// signature matches drivers.Registry.Send so it can back a loadpoint
// SenderFunc directly.
func (s *Server) Command(ctx context.Context, id string, payload []byte) error {
	if s == nil || s.cs == nil {
		return errors.New("ocpp: server not running")
	}
	if !s.handler.IsOnline(id) {
		return fmt.Errorf("%w: %s", ErrNotConnected, id)
	}

	var c command
	if err := json.Unmarshal(payload, &c); err != nil {
		return fmt.Errorf("ocpp: bad command payload for %s: %w", id, err)
	}

	switch c.Action {
	case "init", "deinit":
		// Lifecycle hooks that only mean something to a Lua VM.
		return nil

	case "ev_set_current":
		return s.setLimit(ctx, id, c.amps(), c.numberPhases())

	case "ev_pause":
		// Phase count is meaningless at zero amps.
		return s.setLimit(ctx, id, 0, nil)

	case "ev_start", "ev_resume":
		// A resume without a rate means "as fast as previously allowed".
		amps := c.amps()
		if amps <= 0 {
			amps = s.handler.LastAmps(id, c.ceiling())
		}
		return s.setLimit(ctx, id, amps, c.numberPhases())

	default:
		return fmt.Errorf("ocpp: unknown action %q for %s", c.Action, id)
	}
}

// ceiling is the highest per-phase current this command permits.
func (c command) ceiling() float64 {
	if c.MaxAmpsPerPhase > 0 {
		return c.MaxAmpsPerPhase
	}
	return defaultMaxAmps
}

// amps converts the requested site power into a per-phase current limit.
//
// Below the IEC minimum the result is zero rather than the minimum: when the
// allocator has less than 6 A of headroom to give, rounding up would draw
// current the site fuse was not asked to carry. Refusing to charge is the safe
// direction of error.
func (c command) amps() float64 {
	if c.PowerW <= 0 {
		return 0
	}

	voltage := c.Voltage
	if voltage <= 0 {
		voltage = defaultVoltage
	}
	phases := c.SitePhases
	if phases <= 0 {
		phases = defaultPhases
	}
	if c.PhaseMode == "1p" {
		phases = 1
	}

	amps := c.PowerW / (voltage * float64(phases))
	if ceiling := c.ceiling(); amps > ceiling {
		amps = ceiling
	}
	if amps < minChargeAmps {
		return 0
	}
	return amps
}

// numberPhases is what to declare in the schedule period, or nil to let the
// charger decide. Only a pinned single-phase command is worth stating.
func (c command) numberPhases() *int {
	if c.PhaseMode == "1p" {
		n := 1
		return &n
	}
	return nil
}

// setLimit installs a charge-point-wide default profile capping the per-phase
// current, and blocks until the charger confirms it, the context ends, or the
// command times out.
func (s *Server) setLimit(ctx context.Context, id string, amps float64, numberPhases *int) error {
	if amps < 0 {
		amps = 0
	}

	period := types.NewChargingSchedulePeriod(scheduleStartS, amps)
	// Declared only when the loadpoint pinned single-phase charging. Left
	// unset otherwise so a charger that can switch phases keeps deciding.
	period.NumberPhases = numberPhases
	schedule := types.NewChargingSchedule(types.ChargingRateUnitAmperes, period)
	profile := types.NewChargingProfile(
		ftwProfileID,
		ftwStackLevel,
		types.ChargingProfilePurposeTxDefaultProfile,
		types.ChargingProfileKindAbsolute,
		schedule,
	)

	type result struct {
		conf *smartcharging.SetChargingProfileConfirmation
		err  error
	}
	// Buffered: the library's callback must never block if we have already
	// stopped waiting.
	done := make(chan result, 1)

	err := s.cs.SetChargingProfile(id, func(conf *smartcharging.SetChargingProfileConfirmation, err error) {
		done <- result{conf: conf, err: err}
	}, allConnectors, profile)
	if err != nil {
		return fmt.Errorf("ocpp: send charging profile to %s: %w", id, err)
	}

	timeout := time.NewTimer(commandTimeout)
	defer timeout.Stop()

	select {
	case r := <-done:
		if r.err != nil {
			return fmt.Errorf("ocpp: %s rejected charging profile: %w", id, r.err)
		}
		if r.conf == nil {
			return fmt.Errorf("ocpp: %s returned no charging profile confirmation", id)
		}
		if r.conf.Status != smartcharging.ChargingProfileStatusAccepted {
			return fmt.Errorf("ocpp: %s answered %s to charging profile", id, r.conf.Status)
		}
		// Only a real charging rate is worth remembering. Recording the zero
		// from a pause would erase the rate a later resume is supposed to
		// restore, and the charger would come back at the fallback ceiling
		// instead of where it left off.
		if amps > 0 {
			s.handler.SetLastAmps(id, amps)
		}
		slog.Info("ocpp: charging limit applied", "charger", id, "amps", amps)
		return nil

	case <-ctx.Done():
		return fmt.Errorf("ocpp: charging profile for %s cancelled: %w", id, ctx.Err())

	case <-timeout.C:
		return fmt.Errorf("ocpp: %s did not confirm charging profile within %s", id, commandTimeout)
	}
}

// DefaultMode is what a charger is left in when FTW stops steering it, and
// mirrors the stance every EV driver already takes: hold the last limit.
//
// An EV charger has no autonomous self-consumption mode to fall back to, and
// dropping to zero would strand a driver with an uncharged car because the EMS
// lost contact. Holding the last granted current keeps the site within the
// envelope that was already judged safe.
func (s *Server) DefaultMode(_ context.Context, id string) error {
	slog.Info("ocpp: leaving charger at its last granted limit", "charger", id)
	return nil
}

// IsOnline reports whether a charge point currently holds a WebSocket session.
//
// This is deliberately not the same as having a vehicle plugged in. A default
// charging profile is exactly the thing you set on an idle charger, so control
// gates on the session being live, not on the connector being occupied.
func (h *Handler) IsOnline(id string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.chargers[id]
	return ok && s.online
}

// LastAmps returns the last limit granted to a charger, or fallback if none
// has been set yet.
func (h *Handler) LastAmps(id string, fallback float64) float64 {
	if h == nil {
		return fallback
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.chargers[id]
	if !ok || s.lastAmps <= 0 {
		return fallback
	}
	return s.lastAmps
}

// SetLastAmps records an accepted limit so a later resume can restore it.
func (h *Handler) SetLastAmps(id string, amps float64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.chargers[id]
	if !ok {
		return
	}
	s.lastAmps = amps
}

// Names returns the charge points seen since start, connected or not. main.go
// uses it to decide whether a loadpoint driver name belongs to OCPP.
func (h *Handler) Names() []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.chargers))
	for id := range h.chargers {
		out = append(out, id)
	}
	return out
}
