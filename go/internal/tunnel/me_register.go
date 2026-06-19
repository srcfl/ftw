package tunnel

import (
	"fmt"
	"sort"
	"strings"
)

// MeRegisterSigningString is the canonical message the owner Pi signs (ES256,
// raw R||S over SHA-256) and the relay verifies for POST /me/register. It binds
// the site to the host_id it polls under, at a point in time, so an
// unauthenticated internet caller can never repoint a site's tunnel mapping to
// a host it controls (which would let it MITM the owner's dashboard).
//
// The format is versioned ("v1") so it can evolve without a silent
// Pi↔relay mismatch: a relay on a newer version simply fails to verify an
// old-format signature and rejects the registration, rather than accepting a
// message whose meaning has drifted.
//
// Both ends MUST build the string identically — that is the entire point of
// keeping it here in the shared package rather than duplicating it.
func MeRegisterSigningString(siteID, hostID string, tsMs int64) string {
	return fmt.Sprintf("ftw-me-register:v1:%s:%s:%d", siteID, hostID, tsMs)
}

// MeRegisterSigningStringV2 extends v1 to also cover the published device-key set
// (C1), so a captured registration body can't be replayed with a swapped or added
// device_pubkeys array to inject an attacker key the relay would then trust for
// signaling. The set is canonicalised (sorted, comma-joined) so the Pi and relay
// agree regardless of slice order. The relay verifies v2 when a device_pubkeys
// field is present and, on success, trusts that set; a v1-only signature (old Pi,
// no device-keys) is still accepted but its device_pubkeys (if any were appended
// by an attacker) are IGNORED.
func MeRegisterSigningStringV2(siteID, hostID string, tsMs int64, devicePubkeys []string) string {
	pk := append([]string(nil), devicePubkeys...)
	sort.Strings(pk)
	return fmt.Sprintf("ftw-me-register:v2:%s:%s:%d:%s", siteID, hostID, tsMs, strings.Join(pk, ","))
}

// MeRegisterMaxSkewMs bounds how far the registration timestamp may be from the
// relay's clock (in either direction). Generous enough for unsynced Pi clocks,
// tight enough that a captured registration body stops being replayable soon.
// Replay within the window only re-affirms the legitimate mapping anyway — a
// different host_id needs a fresh signature, which needs the private key.
const MeRegisterMaxSkewMs = 5 * 60 * 1000
