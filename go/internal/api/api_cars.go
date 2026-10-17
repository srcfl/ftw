package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/protocol"
)

// The car ops for the box's own page (docs/cars.md, Ops). A phone sends them
// as cmd frames; the page speaks HTTP, so each op has a route that hands the
// same arguments to the same runtime and answers with the same refusals.

// CarEdits is the part of the cars runtime that changes cars. It is apart
// from CarViews so a fake that only shows cars need not grow these.
type CarEdits interface {
	Add(protocol.Car) (protocol.Car, error)
	UpdateCar(id string, u cars.Update) (protocol.Car, error)
	Remove(id string) error
	SetSessionCar(lpID, sessionID, carID string) (protocol.ChargerCar, error)
}

// carEdits returns the runtime's edits, or answers 503 as the ops answer
// E_UNAVAILABLE {subsystem: cars} on a box without cars.
func (s *Server) carEdits(w http.ResponseWriter) (CarEdits, bool) {
	edits, ok := s.deps.Cars.(CarEdits)
	if !ok || edits == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "this box keeps no cars", "code": appproto.ErrUnavailable, "reason": "cars"})
		return nil, false
	}
	return edits, true
}

// mayKeepCars refuses a caller without ftw.cars.write, which only the owner
// role carries. The LAN's own page has every scope.
func mayKeepCars(w http.ResponseWriter, r *http.Request) bool {
	if caller, ok := apiauth.FromRequest(r); ok && caller.Scopes.Has(protocol.ScopeCarsWrite) {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "only an owner keeps the list of cars"})
	return false
}

// writeCarError answers a car op's error with the op's code and reason, so
// the page can say what went wrong in words.
func writeCarError(w http.ResponseWriter, op string, err error) {
	code, reason, ok := appproto.CarRefusal(err)
	if !ok {
		// A save the box could not confirm: it may hold the change or not.
		slog.Warn("car op not saved", "op", op, "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the box could not confirm the change was saved", "code": appproto.ErrUnavailable, "reason": "persistence_unconfirmed"})
		return
	}
	status := http.StatusConflict
	switch {
	case code == appproto.ErrNotFound:
		status = http.StatusNotFound
	case errors.Is(err, cars.ErrInvalid):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error(), "code": code, "reason": reason})
}

// carArgs reads a route's body as the op's arguments: the map a cmd frame
// carries, so the op's own checks read it.
func carArgs(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var args map[string]any
	if err := readJSON(r, &args); err != nil || args == nil {
		msg := "body must be a JSON object"
		if err != nil {
			msg = err.Error()
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return nil, false
	}
	return args, true
}

// writeBadCarArg refuses one argument as the op does: E_UNKNOWN_OP naming
// it, with the reason the page reads as "check what you typed".
func writeBadCarArg(w http.ResponseWriter, arg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad argument: " + arg,
		"code": appproto.ErrUnknownOp, "reason": "invalid", "arg": arg})
}

// POST /api/cars is car.add. The body is car.add's arguments: the id the
// page minted and the car's fields, in the names of GET /api/cars.
func (s *Server) handleCarAdd(w http.ResponseWriter, r *http.Request) {
	if !mayKeepCars(w, r) {
		return
	}
	edits, ok := s.carEdits(w)
	if !ok {
		return
	}
	args, ok := carArgs(w, r)
	if !ok {
		return
	}
	car, bad := appproto.CarAddArgs(args)
	if bad != "" {
		writeBadCarArg(w, bad)
		return
	}
	added, err := edits.Add(car)
	if err != nil {
		writeCarError(w, protocol.OpCarAdd, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"car": added})
}

// PATCH /api/cars/{id} is car.update: the fields present change, the rest
// stay as they were. The car is the one the path names.
func (s *Server) handleCarUpdate(w http.ResponseWriter, r *http.Request) {
	if !mayKeepCars(w, r) {
		return
	}
	edits, ok := s.carEdits(w)
	if !ok {
		return
	}
	args, ok := carArgs(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	args["id"] = id
	u, bad := appproto.CarUpdateArgs(args)
	if bad != "" {
		writeBadCarArg(w, bad)
		return
	}
	car, err := edits.UpdateCar(id, u)
	if err != nil {
		writeCarError(w, protocol.OpCarUpdate, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"car": car})
}

// DELETE /api/cars/{id} is car.remove. The box keeps the id as removed.
func (s *Server) handleCarRemove(w http.ResponseWriter, r *http.Request) {
	if !mayKeepCars(w, r) {
		return
	}
	edits, ok := s.carEdits(w)
	if !ok {
		return
	}
	if err := edits.Remove(r.PathValue("id")); err != nil {
		writeCarError(w, protocol.OpCarRemove, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/loadpoints/{id}/car is session.car.set, the one tap. The body
// names the plug-in it answers, {session, car_id}; an empty car_id makes the
// session a visitor's.
func (s *Server) handleSessionCarSet(w http.ResponseWriter, r *http.Request) {
	edits, ok := s.carEdits(w)
	if !ok {
		return
	}
	var req struct {
		Session string  `json:"session"`
		CarID   *string `json:"car_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// The same arguments the op requires: a tap names its plug-in, and says
	// which car or none.
	if req.Session == "" || req.CarID == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session and car_id required"})
		return
	}
	held, err := edits.SetSessionCar(r.PathValue("id"), req.Session, *req.CarID)
	if err != nil {
		writeCarError(w, protocol.OpSessionCarSet, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"charger": held})
}
