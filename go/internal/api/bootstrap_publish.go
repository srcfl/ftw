// bootstrap_publish.go
//
// Pi-side self-publish of the signed instance descriptor to the relay's
// /bootstrap during the brief FIRST-ENROLLMENT window (multi-tenant onboarding,
// Task 5). When a LAN operator mints an enroll PIN (handleOwnerEnrollPin) the Pi
// also mints a high-entropy bootstrap_id; while the Pi has ZERO trusted devices
// yet, it parks its descriptor under its site keyed on claimKey =
// hex(sha256(bootstrap_id)) so a fresh browser that holds the bootstrap_id (from
// the URL fragment) can claim it and open the enroll channel without first holding
// a device key. The relay keys the store on the unguessable claimKey, NEVER on the
// PIN — the PIN stays a separate LAN-presence proof the Pi validates on the
// forwarded enroll. The window is self-closing: the relay GCs the blob after its
// TTL, a completed enrollment burns it, and the Pi refuses to re-publish once any
// device is enrolled.
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
	"strconv"
	"strings"
	"time"
)

// bootstrapPublishIO is the PUT {relay}/bootstrap/{site_id} body. It MUST match
// the relay's struct (cmd/ftw-relay/bootstrap_http.go bootstrapPublishIO):
// descriptor is std-base64 of the Pi-signed descriptor JSON; claim_key is
// hex(sha256(bootstrap_id)) — the 256-bit unguessable handle the relay keys the
// store on (NEVER the 6-digit PIN); ts_ms is this Pi's mint time for the relay's
// replay guard; sig is the ES256 raw r||s HEX signature over
// bootstrapPublishSigningString (the same wire form /me/register uses).
type bootstrapPublishIO struct {
	Descriptor string `json:"descriptor"`
	ClaimKey   string `json:"claim_key"`
	TsMs       int64  `json:"ts_ms"`
	Sig        string `json:"sig"`
}

// bootstrapPublishSigningString is the canonical message the Pi signs to authorise
// parking its descriptor under a site. It MUST be byte-identical to the relay's
// reconstruction (cmd/ftw-relay/bootstrap_http.go bootstrapPublishSigningString) —
// versioned and binding (site_id, claim_key, ts_ms, sha256(descriptor)) so a
// captured signature can't be lifted to another site, re-keyed to a different
// claim_key, replayed after its window, or swapped onto a tampered descriptor.
// claim_key is hex(sha256(bootstrap_id)) — the relay never sees the raw bootstrap
// secret. The descriptor is hashed (not inlined) to keep the string bounded.
func bootstrapPublishSigningString(siteID, claimKey string, tsMs int64, descriptor []byte) string {
	dh := sha256.Sum256(descriptor)
	return "ftw-bootstrap:v1:" + siteID + ":" + claimKey + ":" + strconv.FormatInt(tsMs, 10) + ":" + hex.EncodeToString(dh[:])
}

// bootstrapPublishTimeout bounds the single best-effort PUT so a slow/dead relay
// never wedges the goroutine that handleOwnerEnrollPin fired.
const bootstrapPublishTimeout = 10 * time.Second

// publishBootstrapDescriptor parks this Pi's signed instance descriptor under its
// site on the relay so a fresh browser holding the freshly-minted bootstrap_id can
// claim it and enroll its first passkey. The relay keys the store on
// claimKey = hex(sha256(bootstrap_id)); the RAW bootstrap_id never leaves the Pi
// (it goes only to the LAN browser, which derives the same claimKey from the URL
// fragment). Best-effort and NON-BLOCKING by design: every failure path logs and
// returns; it never errors back to the PIN response.
//
// It self-gates to the zero-device first-enrollment window: if ANY trusted device
// is already enrolled, the bootstrap window is closed and we publish nothing
// (re-publishing then would re-open an internet-claimable window for an already
// owned Pi). It also no-ops when no relay is wired (LAN-only deploy) or no signer
// is available (identity load failed on boot).
func (s *Server) publishBootstrapDescriptor(bootstrapID string) {
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

	// claim_key is the relay store key: hex(sha256(bootstrap_id)). The relay never
	// sees the raw bootstrap_id (the browser derives the same digest from the URL
	// fragment), so a leaked store key reveals nothing guessable.
	claimKeyBytes := sha256.Sum256([]byte(bootstrapID))
	claimKey := hex.EncodeToString(claimKeyBytes[:])
	// ts_ms is minted at PUT time so the relay's ±30s replay guard can expire a
	// captured publish body.
	tsMs := time.Now().UnixMilli()

	// OUTER sig: HEX raw r||s over the relay-canonical signing string. This is the
	// /me/register wire form the relay's verifyES256Hex expects — do NOT re-encode.
	sigHex, err := s.deps.InstanceSigner.SignRawHex(bootstrapPublishSigningString(siteID, claimKey, tsMs, descJSON))
	if err != nil {
		slog.Warn("bootstrap-publish: sign publish", "site_id", siteID, "err", err)
		return
	}

	body, err := json.Marshal(bootstrapPublishIO{
		Descriptor: base64.StdEncoding.EncodeToString(descJSON),
		ClaimKey:   claimKey,
		TsMs:       tsMs,
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
