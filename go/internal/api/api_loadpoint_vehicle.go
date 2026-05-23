package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/loadpoint"
)

// POST /api/loadpoints/{id}/force_start fires the generic
// `charge_start` action on the loadpoint's bound vehicle driver,
// bypassing the auto-wake's cooldown + stretched-backoff throttle.
//
// Used when the auto-wake has given up (5+ failed attempts → 10 min
// cooldown) and the operator knows the car is now reachable — e.g.
// they just woke it from the Tesla app or plug-cycled — and want
// charging to resume immediately rather than waiting for the next
// auto-wake window. Bound timeout (15 s) is enough for a Tesla BLE
// proxy hop including a one-shot wake; longer roundtrips return a
// 502 to the caller (driver-internal error strings are intentionally
// not surfaced — they may include endpoint URLs / IPs).
//
// Generic across vehicle drivers: any driver that implements the
// cross-driver `charge_start` (or its `ev_start` alias) action picks
// this up unchanged. The handler delegates to
// loadpoint.Controller.ForceStartVehicle, which owns the throttle
// reset + wake-kick arming so behaviour is identical to the auto-wake
// path minus the cooldown gate.
//
// Status codes:
//
//	400 — missing/empty loadpoint id in path
//	404 — loadpoint id not configured
//	422 — loadpoint exists but no vehicle driver bound
//	502 — driver send hop returned an error (timeout, refused, etc.)
//	503 — loadpoint controller not wired in this build
//	200 — sent
func (s *Server) handleLoadpointForceStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "loadpoint id required"})
		return
	}
	if s.deps.LoadpointCtrl == nil {
		writeJSON(w, 503, map[string]string{"error": "loadpoint controller not configured"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	driver, err := s.deps.LoadpointCtrl.ForceStartVehicle(ctx, id)
	switch {
	case errors.Is(err, loadpoint.ErrForceStartNotReady):
		writeJSON(w, 503, map[string]string{"error": "loadpoint controller not ready"})
		return
	case errors.Is(err, loadpoint.ErrForceStartLoadpointGone):
		writeJSON(w, 404, map[string]string{"error": "loadpoint not found", "loadpoint_id": id})
		return
	case errors.Is(err, loadpoint.ErrForceStartNoVehicleBound):
		writeJSON(w, 422, map[string]string{"error": "no vehicle driver bound to loadpoint", "loadpoint_id": id})
		return
	case err != nil:
		// Generic 502 — do NOT surface the underlying driver error
		// string (may contain endpoint URLs, internal IPs, proxy
		// auth context). The log line on the controller side
		// carries the detail for the operator to inspect.
		writeJSON(w, 502, map[string]string{
			"error":  "vehicle driver send failed",
			"driver": driver,
		})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":             true,
		"loadpoint_id":   id,
		"vehicle_driver": driver,
		"action":         "charge_start",
	})
}
