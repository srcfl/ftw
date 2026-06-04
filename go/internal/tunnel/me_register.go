package tunnel

import "fmt"

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

// MeRegisterMaxSkewMs bounds how far the registration timestamp may be from the
// relay's clock (in either direction). Generous enough for unsynced Pi clocks,
// tight enough that a captured registration body stops being replayable soon.
// Replay within the window only re-affirms the legitimate mapping anyway — a
// different host_id needs a fresh signature, which needs the private key.
const MeRegisterMaxSkewMs = 5 * 60 * 1000
