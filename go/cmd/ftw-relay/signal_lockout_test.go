package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signal_lockout_test.go — FIX-C: the per-source-IP throttle on browser offers
// must NOT let one attacker IP lock a legit browser (different IP) out of
// site:Home. We drive the handler directly with crafted RemoteAddrs (httptest's
// client can't vary the source IP), through the full mux so the limiter +
// ParkOffer backstop both run.

func newLockoutRelay(t *testing.T) http.Handler {
	t.Helper()
	owners := NewOwnerRegistry()
	if err := owners.Register("site:Home", "host-xyz", "deadbeef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r := &Relay{
		Owners:      owners,
		Polls:       NewPollSecrets(),
		Signals:     NewSignalMailbox(),
		PollTimeout: time.Second,
	}
	return r.Handler()
}

// offerFromIP builds a POST /signal/site:Home/offer request whose source IP is
// fixed to ip, with a distinct VALID (16-hex) rendezvous nonce derived from tag,
// and serves it through h.
func offerFromIP(h http.Handler, ip, tag string) int {
	req := httptest.NewRequest(http.MethodPost,
		"/signal/site%3AHome/offer?n="+nonce16(tag), strings.NewReader("OFFER-SDP"))
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// nonce16 maps an arbitrary tag to a deterministic 16-hex-char nonce that passes
// validSignalNonce (8–64 hex). Distinct tags yield distinct nonces.
func nonce16(tag string) string {
	const hexd = "0123456789abcdef"
	var h uint64 = 1469598103934665603 // FNV-1a offset
	for i := 0; i < len(tag); i++ {
		h ^= uint64(tag[i])
		h *= 1099511628211
	}
	out := make([]byte, 16)
	for i := 0; i < 16; i++ {
		out[15-i] = hexd[h&0xf]
		h >>= 4
	}
	return string(out)
}

// TestSignalOffer_PerIP_NoCrossLockout is THE FIX-C regression: an attacker IP
// floods offers for site:Home until it is throttled (429), yet a legit browser on
// a DIFFERENT IP can still park its offer (204). With the old per-site limit the
// attacker's flood would 429 the legit browser too.
func TestSignalOffer_PerIP_NoCrossLockout(t *testing.T) {
	h := newLockoutRelay(t)
	const attacker = "203.0.113.66"
	const victim = "198.51.100.20"

	// Flood from the attacker until it is throttled. The per-site backstop bucket
	// is generous (siteOfferBucketCap), and the per-IP bucket smaller, so the
	// attacker's OWN IP saturates first — proving the bound is per-IP.
	attackerThrottled := false
	for i := 0; i < int(siteOfferBucketCap)+int(offerBucketCapacity)+16; i++ {
		if offerFromIP(h, attacker, "a"+fmtNonceSuffix(i)) == http.StatusTooManyRequests {
			attackerThrottled = true
			break
		}
	}
	if !attackerThrottled {
		t.Fatal("attacker flood was never throttled")
	}

	// THE INVARIANT: the victim, on a different IP, can still park an offer.
	if code := offerFromIP(h, victim, "deadbeef0001"); code != http.StatusNoContent {
		t.Fatalf("FIX-C: legit different-IP browser got %d (want 204) — attacker locked it out of site:Home", code)
	}
}

// TestSignalOffer_PerIP_AttackerBoundedButVictimFlows runs the two interleaved:
// even while the attacker keeps hammering (and getting 429s), the victim's offers
// keep succeeding, bounded only by the victim's own generous per-IP bucket.
func TestSignalOffer_PerIP_AttackerBoundedButVictimFlows(t *testing.T) {
	h := newLockoutRelay(t)
	const attacker = "203.0.113.99"
	const victim = "198.51.100.50"

	// Saturate the attacker first.
	for i := 0; i < int(offerBucketCapacity)+4; i++ {
		offerFromIP(h, attacker, "x"+fmtNonceSuffix(i))
	}
	// Now interleave: attacker is throttled, victim still gets through for its
	// first several (within its own burst).
	victimOK := 0
	for i := 0; i < 4; i++ {
		offerFromIP(h, attacker, "y"+fmtNonceSuffix(i)) // attacker keeps trying (likely 429)
		if offerFromIP(h, victim, "z"+fmtNonceSuffix(i)) == http.StatusNoContent {
			victimOK++
		}
	}
	if victimOK == 0 {
		t.Fatal("FIX-C: victim never got an offer through while the attacker flooded — cross-IP lockout")
	}
}
