// api_owner_access.go
//
// WebAuthn passkey enrollment + login for owner remote access through
// the relay (see docs/goals/relay-as-tunnel.md Phase 3).
//
// Flow:
//   1. /api/owner-access/enroll/start   — operator's browser (relay or LAN)
//      requests a WebAuthn registration challenge.
//   2. /api/owner-access/enroll/finish  — browser POSTs the attestation;
//      host validates, persists in trusted_devices, sets session cookie.
//   3. /api/owner-access/login/start    — browser requests an assertion
//      challenge (allowedCredentials populated from trusted_devices).
//   4. /api/owner-access/login/finish   — browser POSTs the assertion;
//      host validates against the matching credential, sets session
//      cookie, returns success.
//   5. /api/owner-access/devices        — GET lists enrolled devices,
//      DELETE removes one. Both require an authenticated session cookie.
//
// The relay forwards the same /api/owner-access/* paths through the
// tunnel as any other dashboard request (see Phase 2 web proxy).
// Browser-side code lives in web/owner-access/*.{html,js}.
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// ownerAccessCookieName is the session cookie set by the host after a
// successful enrollment or login. The relay does not inspect it — the
// cookie travels through the tunnel back to the host on every request,
// and the host's auth middleware validates it.
const ownerAccessCookieName = "ftw_owner"

// ownerAccessSessionTTL is how long an authenticated session is valid
// after the last successful login.
const ownerAccessSessionTTL = 24 * time.Hour

// ownerAccessCeremonyTTL bounds the time between /enroll/start and
// /enroll/finish (or login start/finish). Browsers typically complete
// the ceremony in <30s; 5 min covers slow Touch-ID prompts.
const ownerAccessCeremonyTTL = 5 * time.Minute

// ownerAccessState is the in-process WebAuthn state. One instance per
// API server; lazy-initialized on first ceremony request.
type ownerAccessState struct {
	mu            sync.Mutex
	wa            *webauthn.WebAuthn
	wsErr         error
	enrollSessions map[string]ceremonySession
	loginSessions  map[string]ceremonySession
	authSessions   map[string]authSession
}

type ceremonySession struct {
	data      *webauthn.SessionData
	createdAt time.Time
}

type authSession struct {
	credentialID []byte
	expiresAt    time.Time
}

// ownerUser implements webauthn.User for the single per-site owner.
// All enrolled passkeys belong to this synthetic user. WebAuthnID is
// the SHA-stable site identifier; WebAuthnName/DisplayName are
// human-readable hints surfaced by the platform's passkey UI.
type ownerUser struct {
	id          []byte
	name        string
	displayName string
	credentials []webauthn.Credential
}

func (u *ownerUser) WebAuthnID() []byte                         { return u.id }
func (u *ownerUser) WebAuthnName() string                       { return u.name }
func (u *ownerUser) WebAuthnDisplayName() string                { return u.displayName }
func (u *ownerUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func (s *Server) ownerAccess() *ownerAccessState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deps.ownerAccess == nil {
		s.deps.ownerAccess = &ownerAccessState{
			enrollSessions: make(map[string]ceremonySession),
			loginSessions:  make(map[string]ceremonySession),
			authSessions:   make(map[string]authSession),
		}
	}
	return s.deps.ownerAccess
}

func (oa *ownerAccessState) webauthnLib(deps *Deps) (*webauthn.WebAuthn, error) {
	oa.mu.Lock()
	defer oa.mu.Unlock()
	if oa.wa != nil || oa.wsErr != nil {
		return oa.wa, oa.wsErr
	}
	rpID := deps.OwnerAccessRPID
	if rpID == "" {
		rpID = "relay.fortytwowatts.com"
	}
	origins := deps.OwnerAccessOrigins
	if len(origins) == 0 {
		origins = []string{"https://" + rpID}
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: "forty-two-watts",
		RPOrigins:     origins,
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		},
	})
	oa.wa, oa.wsErr = wa, err
	return wa, err
}

// buildOwnerUser materializes the owner with all currently-enrolled
// credentials loaded from state.db.
func (s *Server) buildOwnerUser() (*ownerUser, error) {
	if s.deps.State == nil {
		return nil, errors.New("state store not configured")
	}
	devices, err := s.deps.State.LoadTrustedDevices()
	if err != nil {
		return nil, fmt.Errorf("load trusted devices: %w", err)
	}
	creds := make([]webauthn.Credential, 0, len(devices))
	for _, d := range devices {
		creds = append(creds, webauthn.Credential{
			ID:        d.CredentialID,
			PublicKey: d.PublicKey,
			Authenticator: webauthn.Authenticator{
				AAGUID:    d.AAGUID,
				SignCount: d.SignCount,
			},
		})
	}
	id, err := s.ownerWalletHandle()
	if err != nil {
		return nil, fmt.Errorf("owner wallet handle: %w", err)
	}
	return &ownerUser{
		id:          id,
		name:        "owner",
		displayName: ownerDisplayName(s.deps),
		credentials: creds,
	}, nil
}

// resolveDiscoverableOwner is the DiscoverableUserHandler for usernameless
// login: it returns the single owner iff the assertion's userHandle matches
// the stable wallet handle W. The library then matches the credential rawID
// against that owner's enrolled credentials and verifies the signature.
func (s *Server) resolveDiscoverableOwner(rawID, userHandle []byte) (webauthn.User, error) {
	user, err := s.buildOwnerUser()
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(userHandle, user.WebAuthnID()) != 1 {
		return nil, errors.New("owner-access: unknown wallet handle")
	}
	return user, nil
}

func ownerUserID(deps *Deps) []byte {
	if deps != nil && deps.Cfg != nil && deps.Cfg.Site.Name != "" {
		return []byte("site:" + deps.Cfg.Site.Name)
	}
	return []byte("owner")
}

func ownerDisplayName(deps *Deps) string {
	if deps != nil && deps.Cfg != nil && deps.Cfg.Site.Name != "" {
		return deps.Cfg.Site.Name + " operator"
	}
	return "Operator"
}

// ownerWalletHandleKey is the state.db config key holding the stable opaque
// wallet handle W. Minted once, never derived from the mutable site name, so
// renames and name-collisions never orphan enrolled passkeys.
const ownerWalletHandleKey = "owner_wallet_handle"

// ownerWalletHandle returns the stable opaque wallet handle W, minting and
// persisting it on first use.
func (s *Server) ownerWalletHandle() ([]byte, error) {
	if s.deps.State == nil {
		return nil, errors.New("state store not configured")
	}
	if v, ok := s.deps.State.LoadConfig(ownerWalletHandleKey); ok && v != "" {
		return []byte(v), nil
	}
	tok, err := randomToken()
	if err != nil {
		return nil, err
	}
	if err := s.deps.State.SaveConfig(ownerWalletHandleKey, tok); err != nil {
		return nil, err
	}
	return []byte(tok), nil
}

// ---- Helpers ----

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (oa *ownerAccessState) gcCeremonies() {
	cutoff := time.Now().Add(-ownerAccessCeremonyTTL)
	for k, v := range oa.enrollSessions {
		if v.createdAt.Before(cutoff) {
			delete(oa.enrollSessions, k)
		}
	}
	for k, v := range oa.loginSessions {
		if v.createdAt.Before(cutoff) {
			delete(oa.loginSessions, k)
		}
	}
}

func (oa *ownerAccessState) gcAuths() {
	now := time.Now()
	for k, v := range oa.authSessions {
		if v.expiresAt.Before(now) {
			delete(oa.authSessions, k)
		}
	}
}

// authorizeRequest returns the credential_id of the authenticated owner
// for this request, or empty if not authenticated. Reads the session
// cookie; honours LAN-bypass when Deps.OwnerAccessLANBypass is true and
// the request came from a loopback address.
func (s *Server) authorizeOwner(r *http.Request) (credentialID []byte, ok bool) {
	// LAN-bypass applies to genuinely-local requests only. A relay-tunnelled
	// request also lands on a loopback host (the long-poll reverse-proxy
	// connects from 127.0.0.1), so loopback alone is NOT proof of locality —
	// the unforgeable tunnel marker is what distinguishes them.
	if s.deps.OwnerAccessLANBypass && !s.isTunneled(r) {
		return []byte("lan-bypass"), true
	}
	c, err := r.Cookie(ownerAccessCookieName)
	if err != nil {
		return nil, false
	}
	oa := s.ownerAccess()
	oa.mu.Lock()
	defer oa.mu.Unlock()
	oa.gcAuths()
	sess, ok := oa.authSessions[c.Value]
	if !ok {
		return nil, false
	}
	// Renew TTL on use.
	sess.expiresAt = time.Now().Add(ownerAccessSessionTTL)
	oa.authSessions[c.Value] = sess
	return sess.credentialID, true
}

func isLoopback(r *http.Request) bool {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host == "127.0.0.1" || host == "localhost" || host == "[::1]" || host == "::1"
}

// isTunneled reports whether the request arrived via the relay long-poll
// reverse-proxy, which stamps every forwarded request with the per-process
// TunnelMarker secret. Constant-time compare so a direct client cannot probe
// for the secret. A direct client that guesses wrong is simply treated as a
// normal (trusted) LAN client — never an escalation.
func (s *Server) isTunneled(r *http.Request) bool {
	m := s.deps.TunnelMarker
	if m == "" {
		return false
	}
	got := r.Header.Get("X-FTW-Tunnel")
	return subtle.ConstantTimeCompare([]byte(got), []byte(m)) == 1
}

func (s *Server) issueOwnerSession(w http.ResponseWriter, credentialID []byte) error {
	tok, err := randomToken()
	if err != nil {
		return err
	}
	oa := s.ownerAccess()
	oa.mu.Lock()
	oa.authSessions[tok] = authSession{
		credentialID: credentialID,
		expiresAt:    time.Now().Add(ownerAccessSessionTTL),
	}
	oa.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     ownerAccessCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ownerAccessSessionTTL.Seconds()),
	})
	return nil
}

// ---- HTTP handlers ----

// enrollAllowed gates POST /api/owner-access/enroll/start: the first
// enrollment is always allowed (bootstrap); subsequent ones require
// an authenticated session.
func (s *Server) enrollAllowed(r *http.Request) error {
	devices, err := s.deps.State.LoadTrustedDevices()
	if err != nil {
		return fmt.Errorf("check trusted devices: %w", err)
	}
	if len(devices) == 0 {
		// Bootstrap (trust-on-first-use): allowed only from the LAN, never
		// over the relay tunnel where the window would be internet-exposed
		// (whoever reaches enroll/start first on an un-enrolled Pi becomes
		// the owner). The first passkey must be enrolled with physical/LAN
		// presence.
		if s.isTunneled(r) {
			return errors.New("first enrollment must be performed on the local network")
		}
		return nil
	}
	if _, ok := s.authorizeOwner(r); ok {
		return nil
	}
	return errors.New("enrollment requires an existing authenticated session")
}

func (s *Server) handleOwnerEnrollStart(w http.ResponseWriter, r *http.Request) {
	if err := s.enrollAllowed(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	oa := s.ownerAccess()
	wa, err := oa.webauthnLib(s.deps)
	if err != nil {
		http.Error(w, fmt.Sprintf("webauthn init: %v", err), http.StatusInternalServerError)
		return
	}
	user, err := s.buildOwnerUser()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Excludes existing credentials so the same authenticator can't be
	// double-enrolled. The browser will refuse to register if the
	// authenticator is already in the list.
	excludeList := make([]protocol.CredentialDescriptor, 0, len(user.credentials))
	for _, c := range user.credentials {
		excludeList = append(excludeList, protocol.CredentialDescriptor{
			Type:         protocol.PublicKeyCredentialType,
			CredentialID: c.ID,
		})
	}
	options, sessionData, err := wa.BeginRegistration(user,
		webauthn.WithExclusions(excludeList),
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("begin registration: %v", err), http.StatusInternalServerError)
		return
	}
	tok, err := randomToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	oa.mu.Lock()
	oa.gcCeremonies()
	oa.enrollSessions[tok] = ceremonySession{data: sessionData, createdAt: time.Now()}
	oa.mu.Unlock()
	resp := map[string]any{
		"ceremony_token": tok,
		"options":        options,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleOwnerEnrollFinish(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("ceremony_token")
	friendlyName := r.URL.Query().Get("name")
	if tok == "" {
		http.Error(w, "ceremony_token required", http.StatusBadRequest)
		return
	}
	if friendlyName == "" {
		friendlyName = "unnamed device"
	}
	oa := s.ownerAccess()
	oa.mu.Lock()
	sess, ok := oa.enrollSessions[tok]
	if ok {
		delete(oa.enrollSessions, tok)
	}
	oa.mu.Unlock()
	if !ok {
		http.Error(w, "unknown or expired ceremony_token", http.StatusForbidden)
		return
	}
	wa, err := oa.webauthnLib(s.deps)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user, err := s.buildOwnerUser()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cred, err := wa.FinishRegistration(user, *sess.data, r)
	if err != nil {
		http.Error(w, fmt.Sprintf("finish registration: %v", err), http.StatusBadRequest)
		return
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	walletHandle, err := s.ownerWalletHandle()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dev := state.TrustedDevice{
		CredentialID: cred.ID,
		PublicKey:    cred.PublicKey,
		SignCount:    cred.Authenticator.SignCount,
		AAGUID:       cred.Authenticator.AAGUID,
		Transports:   transports,
		FriendlyName: friendlyName,
		CreatedAtMs:  time.Now().UnixMilli(),
		WalletHandle: string(walletHandle),
	}
	if err := s.deps.State.SaveTrustedDevice(dev); err != nil {
		http.Error(w, fmt.Sprintf("save device: %v", err), http.StatusInternalServerError)
		return
	}
	if err := s.issueOwnerSession(w, cred.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"credential_id_b64": base64.RawURLEncoding.EncodeToString(cred.ID),
		"friendly_name":     friendlyName,
	})
}

func (s *Server) handleOwnerLoginStart(w http.ResponseWriter, r *http.Request) {
	oa := s.ownerAccess()
	wa, err := oa.webauthnLib(s.deps)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Still 404 when nothing is enrolled so the landing page shows the
	// "enroll on LAN first" panel.
	devices, err := s.deps.State.LoadTrustedDevices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(devices) == 0 {
		http.Error(w, "no devices enrolled yet", http.StatusNotFound)
		return
	}
	// Usernameless: empty allowCredentials, resolve the user from the
	// assertion's userHandle at finish time.
	options, sessionData, err := wa.BeginDiscoverableLogin()
	if err != nil {
		http.Error(w, fmt.Sprintf("begin discoverable login: %v", err), http.StatusInternalServerError)
		return
	}
	tok, err := randomToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	oa.mu.Lock()
	oa.gcCeremonies()
	oa.loginSessions[tok] = ceremonySession{data: sessionData, createdAt: time.Now()}
	oa.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ceremony_token": tok,
		"options":        options,
	})
}

func (s *Server) handleOwnerLoginFinish(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("ceremony_token")
	if tok == "" {
		http.Error(w, "ceremony_token required", http.StatusBadRequest)
		return
	}
	oa := s.ownerAccess()
	oa.mu.Lock()
	sess, ok := oa.loginSessions[tok]
	if ok {
		delete(oa.loginSessions, tok)
	}
	oa.mu.Unlock()
	if !ok {
		http.Error(w, "unknown or expired ceremony_token", http.StatusForbidden)
		return
	}
	wa, err := oa.webauthnLib(s.deps)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cred, err := wa.FinishDiscoverableLogin(s.resolveDiscoverableOwner, *sess.data, r)
	if err != nil {
		http.Error(w, fmt.Sprintf("finish login: %v", err), http.StatusUnauthorized)
		return
	}
	// Cloned-authenticator guard: sign_count must monotonically increase.
	// The webauthn library has already validated this against
	// sessionData; we additionally persist the new value here so subsequent
	// logins see the latest counter.
	if err := s.deps.State.UpdateTrustedDeviceSignCount(cred.ID, cred.Authenticator.SignCount, time.Now().UnixMilli()); err != nil {
		http.Error(w, fmt.Sprintf("update sign count: %v", err), http.StatusInternalServerError)
		return
	}
	if err := s.issueOwnerSession(w, cred.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"credential_id_b64": base64.RawURLEncoding.EncodeToString(cred.ID),
	})
}

func (s *Server) handleOwnerDevicesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeOwner(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	devices, err := s.deps.State.LoadTrustedDevices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type devOut struct {
		CredentialIDB64 string `json:"credential_id_b64"`
		FriendlyName    string `json:"friendly_name"`
		CreatedAtMs     int64  `json:"created_at_ms"`
		LastUsedMs      int64  `json:"last_used_ms"`
		Transports      []string `json:"transports,omitempty"`
	}
	out := make([]devOut, 0, len(devices))
	for _, d := range devices {
		out = append(out, devOut{
			CredentialIDB64: base64.RawURLEncoding.EncodeToString(d.CredentialID),
			FriendlyName:    d.FriendlyName,
			CreatedAtMs:     d.CreatedAtMs,
			LastUsedMs:      d.LastUsedMs,
			Transports:      d.Transports,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"devices": out})
}

func (s *Server) handleOwnerDeviceDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeOwner(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	credB64 := r.PathValue("credential_id_b64")
	credID, err := base64.RawURLEncoding.DecodeString(credB64)
	if err != nil {
		http.Error(w, "bad credential_id_b64", http.StatusBadRequest)
		return
	}
	if err := s.deps.State.DeleteTrustedDevice(credID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleOwnerWhoami returns 200 + the friendly name of the currently
// authenticated device, or 401 if not authenticated. Used by browser
// landing pages to decide whether to show the login form.
func (s *Server) handleOwnerWhoami(w http.ResponseWriter, r *http.Request) {
	credID, ok := s.authorizeOwner(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	devices, _ := s.deps.State.LoadTrustedDevices()
	var name string
	for _, d := range devices {
		if string(d.CredentialID) == string(credID) {
			name = d.FriendlyName
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"authenticated":    true,
		"friendly_name":    name,
		"devices_enrolled": len(devices),
		"site_id":          string(ownerUserID(s.deps)),
		"wallet":           string(mustWalletHandle(s)),
	})
}

// mustWalletHandle returns the wallet handle for response surfaces that must
// not fail the whole request if state is momentarily unavailable.
func mustWalletHandle(s *Server) []byte {
	w, err := s.ownerWalletHandle()
	if err != nil {
		return nil
	}
	return w
}
