// bootstrap_publish.go
//
// Pi-side self-publish of the signed instance descriptor to the relay's
// /bootstrap during the brief FIRST-ENROLLMENT window (multi-tenant onboarding,
// Task 5). When a LAN operator mints an enroll PIN (handleOwnerEnrollPin) and
// the Pi has ZERO trusted devices yet, the Pi parks its descriptor under its
// site so a fresh browser that knows the PIN can claim it and open the enroll
// channel without first holding a device key. The window is self-closing: the
// relay GCs the blob after its TTL, a completed enrollment burns it, and the Pi
// refuses to re-publish once any device is enrolled.
//
// TWO signatures, TWO encodings — this is the whole point of the file:
//   - INNER descriptor `sig` (base64url, no padding): built by the shared
//     buildInstanceDescriptor so the browser's verifyEntry accepts it BYTE FOR
//     BYTE the same as GET /api/owner-access/instance-descriptor.
//   - OUTER publish `sig` (HEX raw r||s): authenticates the PUT to the relay,
//     reconstructed by the relay's verifyES256Hex against the site's PINNED key
//     over bootstrapPublishSigningString.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// bootstrapPublishIO is the PUT {relay}/bootstrap/{site_id} body. It MUST match
// the relay's struct (cmd/ftw-relay/bootstrap_http.go bootstrapPublishIO):
// descriptor is std-base64 of the Pi-signed descriptor JSON; pin_hash is
// hex(sha256(PIN)); sig is the ES256 raw r||s HEX signature over
// bootstrapPublishSigningString (the same wire form /me/register uses).
type bootstrapPublishIO struct {
	Descriptor string `json:"descriptor"`
	PinHash    string `json:"pin_hash"`
	Sig        string `json:"sig"`
}

// bootstrapPublishSigningString is the canonical message the Pi signs to authorise
// parking its descriptor under a site. It MUST be byte-identical to the relay's
// reconstruction (cmd/ftw-relay/bootstrap_http.go) — versioned and binding
// (site_id, pin_hash, sha256(descriptor)) so a captured signature can't be lifted
// to another site, re-keyed to a different PIN, or swapped onto a tampered
// descriptor. The descriptor is hashed (not inlined) to keep the string bounded.
func bootstrapPublishSigningString(siteID, pinHash string, descriptor []byte) string {
	dh := sha256.Sum256(descriptor)
	return "ftw-bootstrap:v1:" + siteID + ":" + pinHash + ":" + hex.EncodeToString(dh[:])
}

// bootstrapPublishTimeout bounds the single best-effort PUT so a slow/dead relay
// never wedges the goroutine that handleOwnerEnrollPin fired.
const bootstrapPublishTimeout = 10 * time.Second

// publishBootstrapDescriptor parks this Pi's signed instance descriptor under its
// site on the relay so a fresh browser holding the freshly-minted enroll PIN can
// claim it and enroll its first passkey. Best-effort and NON-BLOCKING by design:
// every failure path logs and returns; it never errors back to the PIN response.
//
// It self-gates to the zero-device first-enrollment window: if ANY trusted device
// is already enrolled, the bootstrap window is closed and we publish nothing
// (re-publishing then would re-open an internet-claimable window for an already
// owned Pi). It also no-ops when no relay is wired (LAN-only deploy) or no signer
// is available (identity load failed on boot).
func (s *Server) publishBootstrapDescriptor(pin string) {
	relayBase := strings.TrimRight(s.deps.RelayBaseURL, "/")
	if relayBase == "" {
		return // LAN-only: no relay to publish to.
	}
	if s.deps.State == nil || s.deps.InstanceSigner == nil {
		return
	}
	// Zero-device window only. Grep-mirror of enrollAllowed's LoadTrustedDevices
	// bootstrap gate: once a passkey exists the window is closed.
	devices, err := s.deps.State.LoadTrustedDevices()
	if err != nil {
		slog.Warn("bootstrap-publish: load trusted devices", "err", err)
		return
	}
	if len(devices) > 0 {
		return // window closed — a device is already enrolled.
	}

	siteID := s.deps.SiteID
	descJSON, err := s.buildInstanceDescriptor()
	if err != nil {
		slog.Warn("bootstrap-publish: build descriptor", "site_id", siteID, "err", err)
		return
	}

	pinHashBytes := sha256.Sum256([]byte(pin))
	pinHash := hex.EncodeToString(pinHashBytes[:])

	// OUTER sig: HEX raw r||s over the relay-canonical signing string. This is the
	// /me/register wire form the relay's verifyES256Hex expects — do NOT re-encode.
	sigHex, err := s.deps.InstanceSigner.SignRawHex(bootstrapPublishSigningString(siteID, pinHash, descJSON))
	if err != nil {
		slog.Warn("bootstrap-publish: sign publish", "site_id", siteID, "err", err)
		return
	}

	body, err := json.Marshal(bootstrapPublishIO{
		Descriptor: base64.StdEncoding.EncodeToString(descJSON),
		PinHash:    pinHash,
		Sig:        sigHex,
	})
	if err != nil {
		slog.Warn("bootstrap-publish: marshal body", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootstrapPublishTimeout)
	defer cancel()
	url := relayBase + "/bootstrap/" + siteID
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		slog.Warn("bootstrap-publish: build request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("bootstrap-publish: PUT to relay", "url", url, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("bootstrap-publish: relay rejected", "status", resp.StatusCode, "site_id", siteID)
		return
	}
	slog.Info("bootstrap-publish: descriptor parked on relay", "site_id", siteID)
}
