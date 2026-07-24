package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/srcfl/ftw/go/internal/homelink"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
)

const maxHomeLinkAPIBytes = 20 * 1024

type HomeLinkAdmin interface {
	Status(context.Context) (homelink.AdminStatus, error)
	CreatePairing() (homelink.PairingSetup, error)
	RevokeCredential(context.Context, string) error
}

type homeLinkPasskeyRevokeRequest struct {
	CredentialID string `json:"credential_id"`
}

func (s *Server) handleHomeLinkStatus(w http.ResponseWriter, r *http.Request) {
	if !homeLinkLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "local-only"})
		return
	}
	if s.deps.HomeLink == nil {
		writeJSON(w, http.StatusOK, homelink.AdminStatus{
			Enabled: s.deps.HomeLinkEnabled, Credentials: []homelink.CredentialSummary{},
		})
		return
	}
	status, err := s.deps.HomeLink.Status(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "status-unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleHomeLinkPairing(w http.ResponseWriter, r *http.Request) {
	if !s.homeLinkMutationAllowed(w, r) {
		return
	}
	pairing, err := s.deps.HomeLink.CreatePairing()
	if err != nil {
		writeHomeLinkError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pairing)
}

func (s *Server) handleHomeLinkPasskeyRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.homeLinkMutationAllowed(w, r) {
		return
	}
	var request homeLinkPasskeyRevokeRequest
	if err := readStrictHomeLinkJSON(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid-request"})
		return
	}
	if err := s.deps.HomeLink.RevokeCredential(r.Context(), request.CredentialID); err != nil {
		writeHomeLinkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (s *Server) homeLinkMutationAllowed(w http.ResponseWriter, r *http.Request) bool {
	if !homeLinkLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "local-only"})
		return false
	}
	if s.deps.HomeLink == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "identity-unavailable"})
		return false
	}
	return true
}

func homeLinkLocalRequest(r *http.Request) bool {
	if hasForwardingHeader(r.Header) {
		return false
	}
	authority, err := parseAuthority(r.Host)
	return err == nil && isLocalAuthority(authority) && isLocalClient(r.RemoteAddr)
}

func hasForwardingHeader(header http.Header) bool {
	for key := range header {
		switch {
		case strings.EqualFold(key, "Forwarded"),
			strings.EqualFold(key, "X-Forwarded-For"),
			strings.EqualFold(key, "X-Forwarded-Host"),
			strings.EqualFold(key, "X-Forwarded-Proto"),
			strings.EqualFold(key, "X-Real-IP"):
			return true
		}
	}
	return false
}

func readStrictHomeLinkJSON(r *http.Request, destination any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHomeLinkAPIBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxHomeLinkAPIBytes {
		return errors.New("invalid Home Link request")
	}
	return wire.DecodeStrict(body, maxHomeLinkAPIBytes, destination)
}

func writeHomeLinkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, homelink.ErrRemoteDisabled):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "remote-disabled"})
	case errors.Is(err, homelink.ErrRegistrationDenied),
		errors.Is(err, homelink.ErrCredentialUnknown),
		errors.Is(err, homelink.ErrCredentialRevoked),
		errors.Is(err, homelink.ErrCredentialUncertain):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "denied"})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request-failed"})
	}
}
