package tunnel

import (
	"fmt"
	"strings"
)

// DtlsFingerprintSigningString is the canonical message the owner Pi signs
// (ES256, raw R||S over SHA-256, via nova.Identity.SignRawHex) so the browser
// can verify — against the Pi key it pinned at first connect — that the DTLS
// fingerprint of the WebRTC answer really belongs to THIS Pi. That closes
// relay/TURN man-in-the-middle of the signaling: a key-less relay can swap the
// SDP/fingerprint it relays, but cannot produce a valid signature over the
// substituted fingerprint, so the browser aborts. It is independent of whether
// the bytes later flow direct or via TURN.
//
// fpHex is the answer's SHA-256 DTLS certificate fingerprint as 64 LOWERCASE
// hex chars with NO colons — both ends normalise the SDP "a=fingerprint" line
// identically (Go via NormalizeDtlsFingerprint, the browser in JS).
//
// Versioned + domain-separated ("ftw-dtls-fp:v1:") so it can never collide with
// the same key's other signatures (JWT / claim proof / me-register). See the
// domain-separation note on nova.Identity.SignRawHex.
func DtlsFingerprintSigningString(siteID, fpHex string, tsMs int64) string {
	return fmt.Sprintf("ftw-dtls-fp:v1:%s:%s:%d", siteID, fpHex, tsMs)
}

// DtlsFingerprintMaxSkewMs bounds how fresh the signed answer must be (mirrors
// MeRegisterMaxSkewMs). A captured answer stops being replayable soon, and a
// fresh ICE ufrag/pwd per session means a replayed old answer can't bring up a
// live DTLS channel anyway.
const DtlsFingerprintMaxSkewMs = 5 * 60 * 1000

// NormalizeDtlsFingerprint reduces a DTLS fingerprint HEX TOKEN (the field
// AFTER the algorithm in an SDP "a=fingerprint:sha-256 <token>" line, e.g.
// "AB:CD:EF:..", any case, colon-separated) to 64 lowercase hex chars with no
// colons or spaces — the canonical form fed to DtlsFingerprintSigningString.
//
// Pass ONLY the token, never the whole line: it keeps every hex character and
// drops the rest, and "sha-256" itself contains hex digits (a, 2, 5, 6). The
// browser performs the identical reduction in JS before verifying, so the two
// ends always sign/verify the same string.
func NormalizeDtlsFingerprint(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			b.WriteRune(r)
		case r >= 'A' && r <= 'F':
			b.WriteRune(r + ('a' - 'A'))
		}
	}
	return b.String()
}
