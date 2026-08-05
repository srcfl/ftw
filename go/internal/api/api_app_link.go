package api

import (
	"net/http"
	"strings"
	"time"
)

// Pairing a phone with this box.
//
// The box mints a single-use code and shows the payload as a QR. The app scans
// it, and the box's static key travels optically — never through Sourceful.
// That is what lets a hostile or compelled relay deny service without being
// able to impersonate a box.
//
// Local only, and strictly. Everything below is what a caller needs to add a
// device that can then control the site, so it is reachable from the LAN and
// from nowhere else: no forwarding headers honoured, no remote host accepted.
// The old Home Link admin surface held the same line for the same reason.

// AppEnroller is the box's enrollment identity, as this package needs it.
//
// An interface rather than the concrete type so the API package does not
// depend on appenroll, and so a test can hand in something that fails.
type AppEnroller interface {
	// MintPairingCode issues a fresh single-use code and forgets any previous
	// one, so a code left on a screen stops working as soon as another is
	// asked for.
	MintPairingCode() ([]byte, time.Time, error)
	// EnrollmentURL is what goes in the QR.
	EnrollmentURL(code []byte, lanHint string) (string, error)
	// AuthorisedCount is how many devices may currently connect.
	AuthorisedCount() int
}

type appLinkStatus struct {
	Enabled bool `json:"enabled"`
	// PairedDevices is a count, never a list. A device list on an unauthenticated
	// LAN endpoint is a household inventory.
	PairedDevices int `json:"paired_devices"`
}

type appLinkPairing struct {
	// URL is the whole QR payload. The fragment carries the box's static key,
	// the rendezvous secret and the pairing code; none of it reaches a server.
	URL string `json:"url"`
	// ExpiresAtMs is when the code stops working, so the UI can say so rather
	// than leaving a stale square on screen.
	ExpiresAtMs int64 `json:"expires_at_ms"`
}

func (s *Server) handleAppLinkStatus(w http.ResponseWriter, r *http.Request) {
	if !s.appLinkLocalRequest(w, r) {
		return
	}

	if s.deps.AppEnroll == nil {
		writeJSON(w, http.StatusOK, appLinkStatus{Enabled: false})
		return
	}
	writeJSON(w, http.StatusOK, appLinkStatus{
		Enabled:       true,
		PairedDevices: s.deps.AppEnroll.AuthorisedCount(),
	})
}

func (s *Server) handleAppLinkPairing(w http.ResponseWriter, r *http.Request) {
	if !s.appLinkLocalRequest(w, r) {
		return
	}

	if s.deps.AppEnroll == nil {
		writeAppLinkError(w, http.StatusServiceUnavailable, "app_link is off — set app_link.enabled and restart")
		return
	}

	code, expiresAt, err := s.deps.AppEnroll.MintPairingCode()
	if err != nil {
		writeAppLinkError(w, http.StatusInternalServerError, "could not mint a pairing code")
		return
	}

	// The LAN hint is where the app would reach this box directly. It is
	// advisory and unused today — the relay is the only carrier — but it is
	// part of the payload so a LAN carrier can be added without another
	// payload version, and so a box that knows its address says so.
	url, err := s.deps.AppEnroll.EnrollmentURL(code, s.appLinkLANHint(r))
	if err != nil {
		writeAppLinkError(w, http.StatusInternalServerError, "could not build the pairing code")
		return
	}

	writeJSON(w, http.StatusOK, appLinkPairing{
		URL:         url,
		ExpiresAtMs: expiresAt.UnixMilli(),
	})
}

// appLinkLANHint is the host the caller reached this box on.
//
// Taken from the request rather than from configuration because the box does
// not reliably know its own address, and whatever the browser just used
// demonstrably works from inside the house.
func (s *Server) appLinkLANHint(r *http.Request) string {
	host := r.Host
	if len(host) > 64 {
		return ""
	}
	return host
}

// appLinkLocalRequest refuses anything that did not come from the LAN.
//
// Forwarding headers are grounds for refusal rather than something to parse:
// their presence means a proxy is in the path, and a proxy in front of this
// endpoint means a request from outside the house could look like one from
// inside it.
func (s *Server) appLinkLocalRequest(w http.ResponseWriter, r *http.Request) bool {
	if hasForwardingHeader(r.Header) {
		writeAppLinkError(w, http.StatusForbidden, "pairing is available on your local network only")
		return false
	}
	auth, err := parseAuthority(r.Host)
	if err != nil || !isLocalAuthority(auth) || !isLocalClient(r.RemoteAddr) {
		writeAppLinkError(w, http.StatusForbidden, "pairing is available on your local network only")
		return false
	}
	return true
}

func writeAppLinkError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// hasForwardingHeader reports whether a proxy touched this request.
//
// Their presence is grounds for refusal rather than something to parse: a
// proxy in front of a LAN-only endpoint means a request from outside the house
// can be made to look like one from inside it, and no amount of careful header
// reading fixes that.
func hasForwardingHeader(header http.Header) bool {
	for key := range header {
		switch {
		case strings.EqualFold(key, "Forwarded"),
			strings.EqualFold(key, "X-Real-IP"),
			strings.HasPrefix(strings.ToLower(key), "x-forwarded-"):
			return true
		}
	}
	return false
}
