package api

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
)

// Pairing a phone with this box.
//
// The box mints a single-use code and shows the payload as a QR. The app scans
// it, and the box's static key travels optically — never through Sourceful.
// That is what lets a hostile or compelled relay deny service without being
// able to impersonate a box.
//
// Two doors, and what each one proves decides what it opens. The LAN proves
// PRESENCE — somebody is in the building — and it is the only door that admits
// a new owner or mints a code to read aloud. An app session proves ENROLMENT
// and nothing about location, so it may see the roster, invite a viewer and
// lock a phone out, all of it gated on the members scopes its grant carries.
// See appLinkGate and appLinkRoleAllowed, which are where that is decided.

// AppEnroller is the box's enrollment identity, as this package needs it.
//
// An interface rather than the concrete type so the API package does not
// depend on appenroll, and so a test can hand in something that fails.
type AppEnroller interface {
	// MintPairingCode issues a fresh single-use code for the given role and
	// forgets any previous one, so a code left on a screen stops working as
	// soon as another is asked for.
	MintPairingCode(role string) ([]byte, time.Time, error)
	// MintSpokenCode issues a box code: eight characters somebody can read
	// down a phone. Same single-use, same replacement of any live code.
	MintSpokenCode(role string) (string, time.Time, error)
	// EnrollmentURL is what goes in the QR.
	EnrollmentURL(code []byte, lanHint string) (string, error)
	// AuthorisedCount is how many devices may currently connect.
	AuthorisedCount() int
	// Devices lists the paired phones, most recently seen first.
	Devices() []AppDevice
	// SetDeviceRole changes what one phone may do. Returns
	// ErrLastAppOwnerProtected when it would leave the box with no owner.
	SetDeviceRole(id, role string) error
	// RevokeDevice forgets one and tears down its live sessions. Returns
	// ErrUnknownAppDevice when no row carries the id, and
	// ErrLastAppOwnerProtected when it would leave the box with no owner and
	// the caller is not standing at it.
	//
	// atTheBox is that last fact, and it is the caller's to establish rather
	// than the request's to claim: it comes from which door the request came
	// through, never from anything a remote caller can put in one.
	RevokeDevice(id string, atTheBox bool) error
}

// AppDevice is one paired phone. The id is a short prefix of its key —
// enough to name a row and revoke it, never enough to impersonate it.
type AppDevice struct {
	ID         string `json:"id"`
	AddedAtMs  int64  `json:"added_at_ms,omitempty"`
	LastSeenMs int64  `json:"last_seen_ms,omitempty"`
	// Role is what this phone may do: owner or viewer. Sharing lives in this
	// list rather than on a screen of its own, because a guest's phone is a
	// paired phone and removing one is the same action as locking one out.
	Role string `json:"role"`
	// LastOwner marks the only phone left that can change anything here. It
	// cannot be stepped down, and cannot be removed over a session — so a
	// screen can say what it is before somebody presses a button. The box's
	// own page may remove it, and warns instead of refusing.
	LastOwner bool `json:"last_owner,omitempty"`
}

var (
	// ErrUnknownAppDevice is a revoke aimed at an id no paired phone carries.
	ErrUnknownAppDevice = errors.New("api: no such app device")
	// ErrLastAppOwnerProtected is a change that would leave the box with no
	// owner at all.
	ErrLastAppOwnerProtected = errors.New("api: that is the only owner")
	// ErrUnknownAppRole is a role that is not in contract/registry.yaml.
	ErrUnknownAppRole = errors.New("api: no such role")
)

type appLinkStatus struct {
	Enabled bool `json:"enabled"`
	// PairedDevices is the count; the list lives behind its own endpoint.
	// The list was originally refused as "a household inventory", but this
	// page can already mint a pairing code — the strictly stronger power —
	// so hiding key prefixes from whoever holds it protected nothing, and
	// it made locking a device out impossible, which is a real control a
	// household needs. The rows still carry prefixes, never keys.
	PairedDevices int `json:"paired_devices"`
}

type appLinkPairing struct {
	// URL is the whole QR payload. The fragment carries the box's static key,
	// the rendezvous secret and the pairing code; none of it reaches a server.
	// Empty for a spoken code, which has no payload to scan.
	URL string `json:"url,omitempty"`
	// Code is the box code, grouped as XXXX-XXXX and meant to be read aloud.
	// Empty for a QR code, whose payload must never be offered as text.
	Code string `json:"code,omitempty"`
	// Role is what the code lets in, echoed so the screen can name it in
	// words above the code. A code whose power is invisible is the one that
	// gets read to the wrong person.
	Role string `json:"role"`
	// ExpiresAtMs is when the code stops working, so the UI can say so rather
	// than leaving a stale square on screen.
	ExpiresAtMs int64 `json:"expires_at_ms"`
}

// appLinkPairingRequest is what a page or an app asks for.
//
// Role is required — see appLinkRoleAllowed for why it has no default. Kind
// still has one: a code that is scanned rather than read aloud is the older
// flow and the safer of the two, since its payload never becomes text.
type appLinkPairingRequest struct {
	// Role is "owner" or "viewer". Validated by the enroller against the
	// registry, never against a list written here.
	Role string `json:"role"`
	// Kind is "qr" or "spoken".
	Kind string `json:"kind"`
}

// appLinkRoleRequest changes what one paired phone may do.
type appLinkRoleRequest struct {
	Role string `json:"role"`
}

func (s *Server) handleAppLinkStatus(w http.ResponseWriter, r *http.Request) {
	if !s.appLinkGate(w, r, appproto.ScopeMembersRead) {
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
	if !s.appLinkGate(w, r, appproto.ScopeMembersWrite) {
		return
	}

	if s.deps.AppEnroll == nil {
		writeAppLinkError(w, http.StatusServiceUnavailable, "app_link is off — set app_link.enabled and restart")
		return
	}

	// A body that will not parse says nothing about the role, which is the
	// same position as a body that named none. Both meet the same refusal
	// below rather than being filled in for.
	var req appLinkPairingRequest
	_ = readJSON(r, &req)
	if !s.appLinkRoleAllowed(w, r, req.Role) {
		return
	}
	if !s.appLinkOwnerMintAllowed(w, r, req.Role) {
		return
	}

	// A spoken code is minted at the box and nowhere else.
	//
	// Forty bits are safe to read down a phone line because of what surrounds
	// them, and the load-bearing part is that every minting costs somebody a
	// walk to the box: five wrong guesses burn a code, and asking for another
	// needs a person in the room. A code mintable from a phone anywhere in the
	// world takes that argument away.
	//
	// It would also buy nothing. A typed code carries the pairing code alone —
	// not the box's static key, not the rendezvous secret — so it re-admits a
	// phone that already holds those and cannot let in a guest's phone, which
	// has never seen this box. There is no second, weaker way to mint one.
	if req.Kind == appPairingKindSpoken {
		if appLinkOverSession(r) {
			writeAppLinkError(w, http.StatusForbidden,
				"a code to read aloud is made on the box, at home.")
			return
		}
		code, expiresAt, err := s.deps.AppEnroll.MintSpokenCode(req.Role)
		if err != nil {
			s.writeAppLinkMintError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, appLinkPairing{
			Code:        code,
			Role:        req.Role,
			ExpiresAtMs: expiresAt.UnixMilli(),
		})
		return
	}

	code, expiresAt, err := s.deps.AppEnroll.MintPairingCode(req.Role)
	if err != nil {
		s.writeAppLinkMintError(w, err)
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
		Role:        req.Role,
		ExpiresAtMs: expiresAt.UnixMilli(),
	})
}

// appPairingKindSpoken asks for a code somebody can read aloud.
const appPairingKindSpoken = "spoken"

func (s *Server) writeAppLinkMintError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUnknownAppRole) {
		writeAppLinkError(w, http.StatusBadRequest, "that is not a kind of access this box grants")
		return
	}
	writeAppLinkError(w, http.StatusInternalServerError, "could not mint a pairing code")
}

func (s *Server) handleAppLinkDevices(w http.ResponseWriter, r *http.Request) {
	if !s.appLinkGate(w, r, appproto.ScopeMembersRead) {
		return
	}
	if s.deps.AppEnroll == nil {
		writeAppLinkError(w, http.StatusServiceUnavailable, "app_link is off — set app_link.enabled and restart")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": s.deps.AppEnroll.Devices()})
}

// handleAppLinkDeviceRevoke locks one phone out, guest or owner.
//
// The last owner is the one row where the two doors part. Over a session the
// removal is refused, because a phone that empties the roster from anywhere in
// the world leaves a box nobody can administer and no remote way to mend it.
// At the box it goes through: the person pressing the button is in the
// building, and the same page mints a fresh pairing code, so refusing there
// protects nothing and only strands a household that has lost the phone.
//
// Which door it was comes from the caller the box named at admission, never
// from the request — there is no field a remote caller could set to claim it.
func (s *Server) handleAppLinkDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.appLinkGate(w, r, appproto.ScopeMembersWrite) {
		return
	}
	if s.deps.AppEnroll == nil {
		writeAppLinkError(w, http.StatusServiceUnavailable, "app_link is off — set app_link.enabled and restart")
		return
	}
	id := r.PathValue("id")
	if err := s.deps.AppEnroll.RevokeDevice(id, !appLinkOverSession(r)); err != nil {
		if errors.Is(err, ErrUnknownAppDevice) {
			writeAppLinkError(w, http.StatusNotFound, "that phone is no longer paired")
			return
		}
		if errors.Is(err, ErrLastAppOwnerProtected) {
			writeAppLinkRefusal(w, http.StatusConflict, appproto.ErrLastOwnerProtected,
				"that is the only phone that can change anything here. "+
					"Pair another owner first, then remove this one.")
			return
		}
		writeAppLinkError(w, http.StatusInternalServerError, "could not remove the phone")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// handleAppLinkDeviceRole promotes a guest or steps an owner down to one.
//
// The same list and the same doors as removing a phone. Sharing is not a
// separate feature with a separate screen: it is a role on a row, and a
// household that can see the row can change it or delete it.
//
// Stepping somebody down is reachable over a session; promoting them to owner
// is not, because that is the same power as minting an owner's code and it
// needs somebody at the box either way.
func (s *Server) handleAppLinkDeviceRole(w http.ResponseWriter, r *http.Request) {
	if !s.appLinkGate(w, r, appproto.ScopeMembersWrite) {
		return
	}
	if s.deps.AppEnroll == nil {
		writeAppLinkError(w, http.StatusServiceUnavailable, "app_link is off — set app_link.enabled and restart")
		return
	}

	var req appLinkRoleRequest
	if err := readJSON(r, &req); err != nil {
		writeAppLinkError(w, http.StatusBadRequest, "could not read what to change it to")
		return
	}
	// The same rule as minting, for the same reason: a promotion to owner is
	// a bigger power than an invitation, so it stays at the box.
	if !s.appLinkRoleAllowed(w, r, req.Role) {
		return
	}
	if !s.appLinkOwnerMintAllowed(w, r, req.Role) {
		return
	}

	switch err := s.deps.AppEnroll.SetDeviceRole(r.PathValue("id"), req.Role); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "changed", "role": req.Role})
	case errors.Is(err, ErrUnknownAppDevice):
		writeAppLinkError(w, http.StatusNotFound, "that phone is no longer paired")
	case errors.Is(err, ErrUnknownAppRole):
		writeAppLinkError(w, http.StatusBadRequest, "that is not a kind of access this box grants")
	case errors.Is(err, ErrLastAppOwnerProtected):
		writeAppLinkRefusal(w, http.StatusConflict, appproto.ErrLastOwnerProtected,
			"that is the only phone that can change anything here. "+
				"Pair another owner first, then step this one down.")
	default:
		writeAppLinkError(w, http.StatusInternalServerError, "could not change what this phone may do")
	}
}

// appLinkLANHint is the host the caller reached this box on.
//
// Taken from the request rather than from configuration because the box does
// not reliably know its own address, and whatever the browser just used
// demonstrably works from inside the house.
//
// A session request has no such host to offer. The passthrough builds it with
// Host "localhost" so the API's trust boundary sees a local client, and
// copying that into the payload would hand a guest an address pointing at
// their own phone. No hint is the honest answer: the field is advisory, the
// relay is the only carrier today, and a wrong hint is worse than none.
func (s *Server) appLinkLANHint(r *http.Request) string {
	if appLinkOverSession(r) {
		return ""
	}
	host := r.Host
	if len(host) > 64 {
		return ""
	}
	return host
}

// appLinkGate decides whether a request may reach one of these routes.
//
// There are two doors onto this list and they prove different things.
//
// The LAN door proves PRESENCE: somebody is standing in this building. That
// is the whole authority behind a printed square, a guest pass and a spoken
// code — it is what makes forty bits read down a phone line safe, because
// each minting costs a walk to the box. Two things are refused there, and the
// first is not about addresses: a forwarding header, because a proxy in the
// path means a request from outside the house can be made to look like one
// from inside it, which is grounds for refusal rather than something to
// parse; and a non-local host or client address, the plain case.
//
// The session door proves ENROLMENT: this is a phone the box already trusts,
// authenticated by its Noise static key, carrying a role the box re-reads on
// every request. It proves nothing whatever about where that phone is.
//
// So the doors do not open onto the same things. Both reach the roster, if
// the grant behind them carries the scope — and a viewer's grant carries
// neither members scope, so a guest can neither read the household's list nor
// change it. Only the LAN door admits a new OWNER; see appLinkRoleAllowed.
//
// This used to be one door, and a session was refused outright at it. The
// passthrough's request is built to look local on purpose — Host localhost,
// loopback address, no forwarding header — so an address check alone would
// have let a phone on the other side of the world mint an owner's code. That
// refusal was right about the danger and wrong about the remedy: it left the
// app's whole sharing screen answering 403 to its own owner. What an address
// cannot tell us the grant can, so the grant is what is asked.
func (s *Server) appLinkGate(w http.ResponseWriter, r *http.Request, scope string) bool {
	if hasForwardingHeader(r.Header) {
		writeAppLinkError(w, http.StatusForbidden,
			"pairing is available on your local network only")
		return false
	}

	if appLinkOverSession(r) {
		caller, _ := apiauth.FromRequest(r)
		if !caller.Scopes.Has(scope) {
			// Not "you are on the wrong network": this phone may well be in
			// the kitchen, and it is simply not the owner. The app draws none
			// of these controls for a guest, so arriving here is a phone
			// demoted while its screen was open — which is precisely when a
			// sentence about local networks would be a lie.
			writeAppLinkError(w, http.StatusForbidden,
				"only this home's owner can see or change who has access")
			return false
		}
		return true
	}

	auth, err := parseAuthority(r.Host)
	if err != nil || !isLocalAuthority(auth) || !isLocalClient(r.RemoteAddr) {
		writeAppLinkError(w, http.StatusForbidden,
			"pairing is available on your local network only")
		return false
	}
	return true
}

// appLinkOverSession reports whether a request arrived over an app session
// rather than off the box's own network.
//
// An unnamed caller is a request that never met Authenticate — a handler
// called directly by a test. It is treated as the LAN, which is what it would
// have been named.
func appLinkOverSession(r *http.Request) bool {
	caller, named := apiauth.FromRequest(r)
	return named && caller.Kind != apiauth.KindLAN
}

// appLinkRoleAllowed refuses a role the door in front of it cannot hand out.
//
// Two rules, and neither of them is a default.
//
// The role must be NAMED. It used to fall back to owner when a request left
// it out, on the reasoning that a page which has not been updated should keep
// meaning what it used to mean. What that reasoning costs is a default that
// hands over a house whenever a field goes missing: a caller whose role never
// arrives — because it put it in the query string, or because its body failed
// to parse — mints an owner while believing it asked for a viewer. A request
// that does not say is now asked rather than assumed for.
//
// Over a session the box grants a VIEWER and nothing more. An owner is
// admitted at the box, in the house, because presence is the one thing a
// session cannot prove and the printed square always has. Asking for one from
// the app is refused rather than quietly downgraded: a caller told yes and
// handed something smaller is the same defect as one handed something bigger,
// only pointed the other way, and both end at a screen describing access that
// nobody actually has.
func (s *Server) appLinkRoleAllowed(w http.ResponseWriter, r *http.Request, asked string) bool {
	if asked == "" {
		writeAppLinkError(w, http.StatusBadRequest,
			"say which kind of access this is for")
		return false
	}
	if appLinkOverSession(r) && asked != apiauth.RoleViewer {
		writeAppLinkError(w, http.StatusForbidden,
			"from the app you can let someone view this home. "+
				"Making another owner is done on the box, at home.")
		return false
	}
	return true
}

// appLinkOwnerMintAllowed gates owner-making actions.
//
// When the house password is on, presence on a private address is not
// enough — that is how a guest or a ZeroTier peer turns "I can reach
// :8080" into a Noise owner that works from anywhere. Loopback is the box
// itself; the house-password proof is the LAN door.
//
// When the house password is off, the gate admits private-address
// clients. Operator decision 2026-08-29: with lan_auth off the whole
// dashboard and API already accept every LAN client — settings, control,
// driver install — so refusing only this endpoint blocked the household
// on their own sofa while an actual LAN intruder kept doors that persist
// just as well. The trade is stated plainly: with the password off, LAN
// reach can become a remote owner, as it could before #951. Turning the
// house password on restores the strict gate; that is what it is for.
//
// The first enrollee is an owner whatever the code said (appenroll.Authorise),
// so a viewer mint on an empty box is the same power and uses the same gate.
// A session request is built to look like loopback; it is not the box.
func (s *Server) appLinkOwnerMintAllowed(w http.ResponseWriter, r *http.Request, asked string) bool {
	needsProof := asked == apiauth.RoleOwner || s.appLinkHasNoPairedDevice()
	if !needsProof {
		return true
	}
	if isLoopbackClient(r.RemoteAddr) && !appLinkOverSession(r) {
		return true
	}
	houseOK, checked := lanSecretFrom(r.Context())
	if checked && houseOK {
		return true
	}
	if !s.lanAuthEnabled() && isPrivateClient(r.RemoteAddr) && !appLinkOverSession(r) {
		return true
	}
	writeAppLinkError(w, http.StatusForbidden,
		"making another owner is done on the box, or after the house password is on.")
	return false
}

// lanAuthEnabled mirrors handleAuthStatus's read of api.lan_auth: absent
// config reads as off, the state every box without the opt-in is in.
func (s *Server) lanAuthEnabled() bool {
	if s.deps == nil || s.deps.Cfg == nil || s.deps.CfgMu == nil {
		return false
	}
	s.deps.CfgMu.RLock()
	defer s.deps.CfgMu.RUnlock()
	return s.deps.Cfg.API.LANAuth
}

// isPrivateClient reports whether remoteAddr is a directly connected
// private-network peer: RFC 1918, IPv6 ULA, link-local, or loopback.
// Deliberately excludes CGNAT (100.64/10) — some ISPs put whole
// neighbourhoods there, and this check widens an owner-minting door.
func isPrivateClient(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast()
}

func (s *Server) appLinkHasNoPairedDevice() bool {
	return s.deps != nil && s.deps.AppEnroll != nil && s.deps.AppEnroll.AuthorisedCount() == 0
}

func writeAppLinkError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeAppLinkRefusal is writeAppLinkError plus a name the app can branch on.
//
// Two audiences read these bodies. The box's own page prints the sentence,
// because it is the box's page and there is nobody else to write one. The app
// owns every word it shows and needs the NAME instead: a status alone cannot
// carry it, since 409 is a conflict and nothing more specific, and a sentence
// cannot either — matching on prose is how a wording change becomes a bug in
// another repository.
//
// So: a code wherever the refusal is a rule rather than a mishap, and the code
// comes from contract/registry.yaml through appproto's generated constants.
// Never a literal here. The app's own screen for this was dead for a release
// because it read a "code" key this floor had never sent.
func writeAppLinkRefusal(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// hasForwardingHeader reports whether a proxy touched this request.
//
// Their presence is grounds for refusal rather than something to parse: a
// proxy in front of a door that decides presence from an address means a
// request from outside the house can be made to look like one from inside it,
// and no amount of careful header reading fixes that.
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
