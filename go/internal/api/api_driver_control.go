// Operator-facing driver controls: send one declared command, hold it for a
// bounded time, and hand the device back to itself when the hold ends.
//
// Deliberately outside the signed runtime policy. A commandable driver has no
// policy, and giving it a synthesised one would be worse than doing nothing:
// HostEnv.permissionAllowed grants everything only while the policy is nil,
// so a policy without permissions silently blocks the driver's own MQTT, and
// any policy denies every write. So this path leaves the policy layer
// untouched and validates against the driver's catalog declaration instead.
//
// What that costs is honest and worth stating: no host-enforced write scope
// and no host-verified evidence. What it keeps is the part that protects
// hardware — Core clamps every value to the declared bounds rather than
// trusting the Lua to do it, and every hold ends by itself.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/srcfl/ftw/go/internal/drivercontrol"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// A hold has to end on its own. An offset left behind by a browser tab that
// closed, or by an FTW that stopped answering, is a house heated wrong for
// weeks — the failure nobody notices until the bill. 24 h is long enough for
// "warm through the cold snap" and short enough that forgetting is cheap.
const (
	maxControlHoldSeconds     = 24 * 60 * 60
	defaultControlHoldSeconds = 4 * 60 * 60
)

type controlRequest struct {
	Control   string          `json:"control"`
	Value     json.RawMessage `json:"value"`
	DurationS int             `json:"duration_s"`
}

// POST /api/drivers/{name}/control — send one declared command and hold it.
//
// The command must be declared by the driver's catalog entry. That is the
// whole allowlist: a driver that declares nothing can be commanded by nobody,
// and a typo'd control name is a 400 rather than a silent success. The Lua
// command hook returns no value for an action it does not know, which the
// registry cannot tell apart from a command that worked.
func (s *Server) handleDriverControl(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "missing driver name"})
		return
	}
	var req controlRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	if cfg, ok := s.configuredDriver(name); ok && cfg.ObserveOnly {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":        "driver is observe_only and cannot be controlled",
			"observe_only": true,
		})
		return
	}

	declared := s.driverControls(name)
	if len(declared) == 0 {
		writeJSON(w, 404, map[string]string{"error": "driver declares no controls"})
		return
	}
	var control *drivers.CatalogControl
	for i := range declared {
		if declared[i].ID == req.Control {
			control = &declared[i]
			break
		}
	}
	if control == nil {
		writeJSON(w, 400, map[string]string{"error": "unknown control"})
		return
	}
	if s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "driver registry not available"})
		return
	}
	applied, err := decodeControlValue(req.Value, control.Input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	payload := map[string]any{"action": control.ID, "value": applied}
	if value, ok := applied.(float64); ok {
		// Drivers written before this endpoint read their own key names.
		// Sending both costs one JSON field and saves every such driver a
		// rewrite; heishamon reads cmd.offset or cmd.value.
		payload["offset"] = value
	}

	body, err := json.Marshal(payload)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	seconds := req.DurationS
	if seconds <= 0 {
		seconds = defaultControlHoldSeconds
	}
	if seconds > maxControlHoldSeconds {
		seconds = maxControlHoldSeconds
	}

	// Allow lifecycle tests to pause at the same boundary as a real request.
	// The registry lookup happens after this hook, so a restart here cannot make
	// the request carry a stale generation into the hold.
	if s.beforeDriverControlSend != nil {
		s.beforeDriverControlSend()
	}

	// drivercontrol.Holds.Send keeps dispatch and the hold transition under
	// one per-driver lock and binds the hold to the generation that took the
	// command. The response is written inside done, still under that lock, so
	// the status read that explains an error is the one that caused it.
	s.controlHolds.Send(r.Context(), name, control.ID, applied, body,
		time.Duration(seconds)*time.Second, s.beforeDriverControlStateLock,
		func(hold drivercontrol.Hold, err error) {
			if err != nil {
				s.writeControlSendError(w, name, err)
				return
			}
			writeJSON(w, 200, map[string]any{
				"control":       control.ID,
				"applied":       applied,
				"evidence":      control.Evidence,
				"expires_at_ms": hold.ExpiresAt,
			})
		})
}

func (s *Server) writeControlSendError(w http.ResponseWriter, name string, err error) {
	if errors.Is(err, drivers.ErrObserveOnly) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":        err.Error(),
			"observe_only": true,
		})
		return
	}
	if errors.Is(err, drivers.ErrControlBlocked) {
		status, _ := s.deps.Registry.ControlStatus(name)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":             err.Error(),
			"control_blocked":   true,
			"default_confirmed": false,
			"recovery_pending":  status.RecoveryPending,
		})
		return
	}
	if _, running := s.deps.Registry.ControlStatus(name); !running {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "driver not running"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// DELETE /api/drivers/{name}/control — end the hold now.
//
// Releasing means calling the driver's own default mode, not writing a value
// this package invented. Only the driver knows what neutral is: heishamon's
// is its configured safe_offset, which an operator may have moved.
func (s *Server) handleDriverControlRelease(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "missing driver name"})
		return
	}
	if s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "driver registry not available"})
		return
	}
	if err := s.SendDriverDefault(context.Background(), name); err != nil {
		// An observe_only driver is never commanded, not even back to its
		// default; that is a refusal, not a server error.
		if errors.Is(err, drivers.ErrObserveOnly) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "observe_only": true})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "released"})
}

func decodeControlValue(raw json.RawMessage, in drivers.CatalogControlInput) (any, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("control requires a %s value", in.Type)
	}
	switch in.Type {
	case "number":
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, errors.New("control requires a numeric value")
		}
		return clampToDeclared(value, in), nil
	case "boolean":
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, errors.New("control requires a boolean value")
		}
		return value, nil
	case "string":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, errors.New("control requires a string value")
		}
		// A declared closed set is the whole allowlist, as the declared
		// bounds are for a number. Unlike a number there is no nearest
		// value to clamp to, so anything else is refused.
		if len(in.Options) > 0 && !slices.Contains(in.Options, value) {
			return nil, errors.New("control value is not one of the declared options")
		}
		return value, nil
	default:
		return nil, fmt.Errorf("control has unsupported input type %q", in.Type)
	}
}

func clampToDeclared(value float64, in drivers.CatalogControlInput) float64 {
	if in.Min != nil && value < *in.Min {
		value = *in.Min
	}
	if in.Max != nil && value > *in.Max {
		value = *in.Max
	}
	return value
}

// controlRegistry is what the holds see of the registry. A nil
// *drivers.Registry must stay a nil interface, or the holds would call it.
func (s *Server) controlRegistry() drivercontrol.Registry {
	if s.deps == nil || s.deps.Registry == nil {
		return nil
	}
	return s.deps.Registry
}

// SendDriverDefault is the shared safety path for watchdogs and API release:
// it clears any hold under the per-driver lock and sends the driver's own
// default. See drivercontrol.Holds.SendDefault.
func (s *Server) SendDriverDefault(ctx context.Context, name string) error {
	return s.controlHolds.SendDefault(ctx, name, s.beforeDriverDefaultStateLock)
}
