package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// FTW Cloud over HTTP.
//
// A grant lets the box send a readable copy of the house to Sourceful, so
// an owner must accept it. LAN consent follows the private network and
// optional house-password rules used by Settings and owner pairing.
// Turning Cloud off narrows access, so any local client may do that.
// The box cannot verify the app's step-up, so a grant requires an owner's
// session without claiming a separate step-up. Statistics use the same check.

// CloudGrant is the box's FTW Cloud link as the routes see it.
type CloudGrant interface {
	Status() cloudlink.Status
	Preview() cloudlink.Preview
	Grant(cloudlink.GrantRequest) error
	Revoke(by string) error
	Stats(cloudlink.StatsRequest) error
	StopStats(by string) error
	End(by string) error
}

func (s *Server) cloudRoutes() {
	s.handle("GET  /api/cloud", Read, s.handleCloudStatus)
	s.handle("DELETE /api/cloud", Configure, s.handleCloudEnd, NoStepUp)
	s.handle("GET  /api/cloud/preview", Read, s.handleCloudPreview)
	s.handle("GET /api/cloud/stats-policy", Read, s.handleCloudStatsPolicy)
	s.handle("POST   /api/cloud/grant", Configure, s.handleCloudGrant, NoStepUp)
	s.handle("DELETE /api/cloud/grant", Configure, s.handleCloudRevoke, NoStepUp)
	s.handle("POST   /api/cloud/stats", Configure, s.handleCloudStats, NoStepUp)
	s.handle("DELETE /api/cloud/stats", Configure, s.handleCloudStopStats, NoStepUp)
}

// cloudGate admits an owner's session, and on the LAN a local client. A grant
// follows the private LAN's settings rule: the house password is optional,
// but when it is on the caller must have proved it.
func (s *Server) cloudGate(w http.ResponseWriter, r *http.Request, toGrant bool) bool {
	if hasForwardingHeader(r.Header) {
		writeAppLinkError(w, http.StatusForbidden, "FTW Cloud is managed in the app or on your home network")
		return false
	}
	if appLinkOverSession(r) {
		caller, _ := apiauth.FromRequest(r)
		if caller.Role != apiauth.RoleOwner {
			writeAppLinkError(w, http.StatusForbidden, "only this home's owner can see or change FTW Cloud")
			return false
		}
		return true
	}
	auth, err := parseAuthority(r.Host)
	if err != nil || !isLocalAuthority(auth) || !isLocalClient(r.RemoteAddr) {
		writeAppLinkError(w, http.StatusForbidden, "FTW Cloud is managed in the app or on your home network")
		return false
	}
	if !toGrant || isLoopbackClient(r.RemoteAddr) {
		return true
	}
	if ok, checked := lanSecretFrom(r.Context()); checked && ok {
		return true
	}
	if !s.lanAuthEnabled() && isPrivateClient(r.RemoteAddr) {
		return true
	}
	writeAppLinkError(w, http.StatusForbidden,
		"log in with the house password to save this choice, or use the owner's app")
	return false
}

// cloudCaller names the door for the audit line.
func cloudCaller(r *http.Request) string {
	if caller, named := apiauth.FromRequest(r); named && caller.Kind != apiauth.KindLAN {
		return caller.Subject
	}
	if isLoopbackClient(r.RemoteAddr) {
		return "box"
	}
	return "lan"
}

func (s *Server) cloudLink(w http.ResponseWriter) (CloudGrant, bool) {
	if s.deps == nil || s.deps.Cloud == nil {
		writeAppLinkError(w, http.StatusServiceUnavailable, "FTW Cloud is not available on this box")
		return nil, false
	}
	return s.deps.Cloud, true
}

// GET /api/cloud — the grant, the link and the bytes sent in the last 24
// hours of uptime.
func (s *Server) handleCloudStatus(w http.ResponseWriter, r *http.Request) {
	if !s.cloudGate(w, r, false) {
		return
	}
	link, ok := s.cloudLink(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, link.Status())
}

// GET /api/cloud/preview — the exact bytes that leave the house: the last
// frame of each class, or before a grant what the box would send.
func (s *Server) handleCloudPreview(w http.ResponseWriter, r *http.Request) {
	if !s.cloudGate(w, r, false) {
		return
	}
	link, ok := s.cloudLink(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, link.Preview())
}

// POST /api/cloud/grant {scopes, policy}
func (s *Server) handleCloudGrant(w http.ResponseWriter, r *http.Request) {
	if !s.cloudGate(w, r, true) {
		return
	}
	link, ok := s.cloudLink(w)
	if !ok {
		return
	}
	var req cloudlink.GrantRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeAppLinkError(w, http.StatusBadRequest, "send {scopes, policy}")
		return
	}
	req.By = cloudCaller(r)
	switch err := link.Grant(req); {
	case err == nil:
		writeJSON(w, http.StatusOK, link.Status())
	case errors.Is(err, cloudlink.ErrGrantRequest):
		writeAppLinkError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, cloudlink.ErrPurgePending), errors.Is(err, cloudlink.ErrLinkEnding):
		writeAppLinkError(w, http.StatusConflict, err.Error())
	case errors.Is(err, cloudlink.ErrNoService):
		writeAppLinkError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeAppLinkError(w, http.StatusInternalServerError, "could not save Cloud terms. Try again.")
	}
}

// DELETE /api/cloud/grant — FTW Cloud off, and the service asked to delete
// its copy.
func (s *Server) handleCloudRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.cloudGate(w, r, false) {
		return
	}
	link, ok := s.cloudLink(w)
	if !ok {
		return
	}
	if err := link.Revoke(cloudCaller(r)); err != nil {
		writeAppLinkError(w, http.StatusInternalServerError, "could not turn FTW Cloud off")
		return
	}
	writeJSON(w, http.StatusOK, link.Status())
}

// POST /api/cloud/stats {policy} — contribute to statistics, a separate
// consent under a grant already on. Same gate as the grant.
func (s *Server) handleCloudStats(w http.ResponseWriter, r *http.Request) {
	if !s.cloudGate(w, r, true) {
		return
	}
	link, ok := s.cloudLink(w)
	if !ok {
		return
	}
	var req cloudlink.StatsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeAppLinkError(w, http.StatusBadRequest, "send {policy}")
		return
	}
	req.By = cloudCaller(r)
	switch err := link.Stats(req); {
	case err == nil:
		writeJSON(w, http.StatusOK, link.Status())
	case errors.Is(err, cloudlink.ErrGrantRequest):
		writeAppLinkError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, cloudlink.ErrNotGranted), errors.Is(err, cloudlink.ErrPurgePending), errors.Is(err, cloudlink.ErrCloudTermsFirst):
		writeAppLinkError(w, http.StatusConflict, err.Error())
	case errors.Is(err, cloudlink.ErrNoService):
		writeAppLinkError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeAppLinkError(w, http.StatusInternalServerError, "could not turn statistics on")
	}
}

// DELETE /api/cloud/stats — stop contributing to statistics. Any door, as for
// the grant.
func (s *Server) handleCloudStopStats(w http.ResponseWriter, r *http.Request) {
	if !s.cloudGate(w, r, false) {
		return
	}
	link, ok := s.cloudLink(w)
	if !ok {
		return
	}
	if err := link.StopStats(cloudCaller(r)); err != nil {
		if errors.Is(err, cloudlink.ErrCloudNeedsStatistics) {
			writeAppLinkError(w, http.StatusConflict, err.Error())
			return
		}
		writeAppLinkError(w, http.StatusInternalServerError, "could not turn statistics off")
		return
	}
	writeJSON(w, http.StatusOK, link.Status())
}

// CloudLoadpoints is the loadpoint state GET /api/loadpoints composes, and
// the control feedback beside it, for the FTW Cloud uplink. ok is false on a
// box without loadpoints.
//
// The decoration order is handleLoadpoints'. #1337 moves that sequence into
// one shared function; this should call it once that lands.
func (s *Server) CloudLoadpoints(now time.Time) (states []loadpoint.State, feedback []ControlFeedback, ok bool) {
	if s.deps == nil || s.deps.Loadpoints == nil {
		return nil, nil, false
	}
	states = s.deps.Loadpoints.States()
	decorateLoadpointsWithCar(states, s.deps.Cars, now)
	s.applyPlanningSoC(states, now)
	s.decorateLoadpointsWithManual(states)
	s.decorateLoadpointsWithBatteryBoost(states)
	s.decorateLoadpointsWithPlan(states)
	return states, s.controlFeedback(now), true
}

func (s *Server) handleCloudEnd(w http.ResponseWriter, r *http.Request) {
	if !s.cloudGate(w, r, false) {
		return
	}
	link, ok := s.cloudLink(w)
	if !ok {
		return
	}
	if err := link.End(cloudCaller(r)); err != nil {
		writeAppLinkError(w, http.StatusInternalServerError, "could not stop FTW Cloud")
		return
	}
	writeJSON(w, http.StatusOK, link.Status())
}
