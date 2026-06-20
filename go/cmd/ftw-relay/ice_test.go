package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestSignalICE_DefaultSTUNOnly(t *testing.T) {
	r := &Relay{ICEStunURLs: []string{"stun:one.example:19302"}}
	req := httptest.NewRequest(http.MethodGet, "/signal/ice", nil)
	rr := httptest.NewRecorder()

	r.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var out signalICEWire
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.ICEServers) != 1 {
		t.Fatalf("ice servers = %d, want 1", len(out.ICEServers))
	}
	if got := out.ICEServers[0].URLs; len(got) != 1 || got[0] != "stun:one.example:19302" {
		t.Fatalf("stun urls = %v", got)
	}
	if out.ICEServers[0].Username != "" || out.ICEServers[0].Credential != "" {
		t.Fatalf("STUN entry should not carry TURN credentials: %+v", out.ICEServers[0])
	}
}

func TestSignalICE_TURNRestCredentials(t *testing.T) {
	r := &Relay{
		ICEStunURLs: []string{"stun:one.example:19302"},
		TURNURLs:    []string{"turn:relay.example:3478?transport=udp", "turns:relay.example:5349"},
		TURNSecret:  "shared-with-coturn",
	}
	req := httptest.NewRequest(http.MethodGet, "/signal/ice", nil)
	rr := httptest.NewRecorder()

	r.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out signalICEWire
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.ICEServers) != 2 {
		t.Fatalf("ice servers = %d, want STUN + TURN", len(out.ICEServers))
	}
	turn := out.ICEServers[1]
	if len(turn.URLs) != 2 || turn.URLs[0] != "turn:relay.example:3478?transport=udp" || turn.URLs[1] != "turns:relay.example:5349" {
		t.Fatalf("turn urls = %v", turn.URLs)
	}
	if turn.TTL != int(turnCredentialTTL.Seconds()) {
		t.Fatalf("ttl = %d, want %d", turn.TTL, int(turnCredentialTTL.Seconds()))
	}
	exp, err := strconv.ParseInt(turn.Username, 10, 64)
	if err != nil {
		t.Fatalf("username should be unix expiry: %v", err)
	}
	expiry := time.Unix(exp, 0)
	if expiry.Before(time.Now().Add(turnCredentialTTL-time.Minute)) || expiry.After(time.Now().Add(turnCredentialTTL+time.Minute)) {
		t.Fatalf("expiry = %s, want about %s from now", expiry, turnCredentialTTL)
	}
	mac := hmac.New(sha1.New, []byte("shared-with-coturn"))
	_, _ = mac.Write([]byte(turn.Username))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); turn.Credential != want {
		t.Fatalf("credential HMAC mismatch")
	}
}

func TestSignalICE_HomeHostRoute(t *testing.T) {
	r := &Relay{
		HomeHost:    "home.example.test",
		HomeSite:    "site:Home",
		ICEStunURLs: []string{"stun:home.example:19302"},
	}
	req := httptest.NewRequest(http.MethodGet, "https://home.example.test/signal/ice", nil)
	rr := httptest.NewRecorder()

	r.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("home-host /signal/ice status = %d, want 200", rr.Code)
	}
}

// TestSignalICE_PerIPThrottle guards the per-source-IP rate limit on the
// otherwise-unauthenticated /signal/ice: it mints a fresh coturn credential on
// every call, so a single IP must not be able to pull them in an unbounded loop.
func TestSignalICE_PerIPThrottle(t *testing.T) {
	r := &Relay{
		ICEStunURLs: []string{"stun:one.example:19302"},
		TURNURLs:    []string{"turn:relay.example:3478?transport=udp"},
		TURNSecret:  "shared-with-coturn",
	}
	h := r.Handler() // one handler so all requests share r.OfferLimit

	var first, throttled int
	for i := 0; i < int(iceBucketCapacity)+8; i++ {
		req := httptest.NewRequest(http.MethodGet, "/signal/ice", nil)
		req.RemoteAddr = "203.0.113.9:5555" // same source IP for every request
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if i == 0 {
			first = rr.Code
		}
		if rr.Code == http.StatusTooManyRequests {
			throttled++
		}
	}
	if first != http.StatusOK {
		t.Fatalf("first request = %d, want 200", first)
	}
	if throttled == 0 {
		t.Fatalf("expected at least one 429 once the per-IP burst was exhausted, got none")
	}
}
