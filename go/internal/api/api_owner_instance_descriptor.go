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
	piPubkey := s.deps.InstanceSigner.PublicKeyHex()
	siteID := s.deps.SiteID
	label := siteID
	if s.deps.Cfg != nil && s.deps.Cfg.Site.Name != "" {
		label = s.deps.Cfg.Site.Name
	}
	msg := instanceDescriptorSigningString(siteID, piPubkey, label)
	sigHex, err := s.deps.InstanceSigner.SignRawHex(msg)
	if err != nil {
		slog.Error("instance-descriptor: sign failed", "site_id", siteID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sign descriptor"})
		return
	}
	// SignRawHex returns raw r||s as hex; the CONTRACT wire form is base64url
	// (no padding) so the browser can verify with WebCrypto (P-256 native r||s).
	// Re-encode.
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		slog.Error("instance-descriptor: malformed signature hex", "site_id", siteID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "encode descriptor"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site_id":   siteID,
		"pi_pubkey": piPubkey,
		"label":     label,
		"sig":       base64.RawURLEncoding.EncodeToString(sigBytes),
	})
}
