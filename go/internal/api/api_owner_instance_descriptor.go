// api_owner_instance_descriptor.go
//
// GET /api/owner-access/instance-descriptor — the Pi-signed instance descriptor
// the multi-tenant home route relies on. Served over the owner-authenticated P2P
// (DTLS) channel; NOT an open path (deliberately absent from
// isOwnerAccessOpenPath, so the global gate also refuses it without a session).
// The browser stores the {site_id, pi_pubkey, label, sig} tuple inside its
// encrypted directory blob and verifies sig against pi_pubkey
// (web/owner-access/instance-sync.js verifyEntry) before trusting an entry — so
// even a tampering relay that stores the blob cannot inject a fake instance. The
// signing string is domain-separated ("ftw-instance:v1:") and bound to
// site_id + pi_pubkey + label, so a signature minted here can never be replayed
// for another purpose (cf. SignRawHex's domain-separation note in
// internal/nova/identity.go).
package api

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// instanceDescriptorSigningString is the canonical message the Pi signs for the
// instance descriptor. Both ends (Pi here, browser in instance-sync.js
// instanceMessage) MUST build it identically — pinning the format in one place
// is the entire point.
func instanceDescriptorSigningString(siteID, piPubkey, label string) string {
	return "ftw-instance:v1:" + siteID + ":" + piPubkey + ":" + label
}

// buildInstanceDescriptor materializes the Pi-signed instance descriptor as the
// marshaled JSON {site_id, pi_pubkey, label, sig}. It is the SINGLE source of the
// descriptor bytes the browser's verifyEntry trusts, shared by the GET endpoint
// (handleOwnerInstanceDescriptor) and the /bootstrap self-publish
// (publishBootstrapDescriptor) so both produce byte-identical, identically-signed
// descriptors. The inner `sig` is base64url (no padding) of the raw r||s
// signature — the WebCrypto-native form, NOT hex. Returns an error if no signer
// is wired or signing/encoding fails; callers map that to 503 / a skipped publish.
func (s *Server) buildInstanceDescriptor() ([]byte, error) {
	if s.deps.InstanceSigner == nil {
		return nil, errors.New("site identity unavailable")
	}
	piPubkey := s.deps.InstanceSigner.PublicKeyHex()
	siteID := s.deps.SiteID
	label := siteID
	if s.deps.Cfg != nil && s.deps.Cfg.Site.Name != "" {
		label = s.deps.Cfg.Site.Name
	}
	msg := instanceDescriptorSigningString(siteID, piPubkey, label)
	sigHex, err := s.deps.InstanceSigner.SignRawHex(msg)
	if err != nil {
		return nil, err
	}
	// SignRawHex returns raw r||s as hex; the CONTRACT wire form is base64url
	// (no padding) so the browser can verify with WebCrypto (P-256 native r||s).
	// Re-encode.
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"site_id":   siteID,
		"pi_pubkey": piPubkey,
		"label":     label,
		"sig":       base64.RawURLEncoding.EncodeToString(sigBytes),
	})
}

// handleOwnerInstanceDescriptor returns the Pi-signed instance descriptor. Owner
// auth required (same posture as whoami): the descriptor is served only over the
// already-owner-authenticated P2P channel, never anonymously over the relay.
func (s *Server) handleOwnerInstanceDescriptor(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeOwner(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.deps.InstanceSigner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "site identity unavailable"})
		return
	}
	descJSON, err := s.buildInstanceDescriptor()
	if err != nil {
		slog.Error("instance-descriptor: build failed", "site_id", s.deps.SiteID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sign descriptor"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(descJSON)
}
