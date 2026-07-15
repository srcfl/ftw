package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/nova"
	"github.com/srcfl/ftw/go/internal/tunnel"
)

// newTestIdentity mints a throwaway ES256 site identity (the same type the Pi
// signs /me/register with), so the relay's verify path is exercised against the
// real wire format rather than a hand-rolled stand-in.
func newTestIdentity(t *testing.T) *nova.Identity {
	t.Helper()
	id, err := nova.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "k.pem"))
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return id
}

// signedMeBody builds a signed POST /me/register body for the given identity.
func signedMeBody(t *testing.T, id *nova.Identity, siteID, hostID string, tsMs int64) []byte {
	t.Helper()
	sig, err := id.SignRawHex(tunnel.MeRegisterSigningString(siteID, hostID, tsMs))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	b, _ := json.Marshal(meRegisterRequest{
		SiteID: siteID, HostID: hostID,
		PublicKey: id.PublicKeyHex(), TsMs: tsMs, Sig: sig,
	})
	return b
}

// TestMeRegisterReturnsPollSecret confirms the control-plane registration: an
// ES256-signed /me/register pins the site key and returns the per-host poll
// secret. The owner HTTP request/response tunnel (/me/<site>/...) was REMOVED in
// the P2P-only cutover (slice 6) — owner data rides the DTLS DataChannel only —
// so this test no longer forwards any owner request through the relay.
func TestMeRegisterReturnsPollSecret(t *testing.T) {
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		Owners:      NewOwnerRegistry(),
		PollTimeout: 1 * time.Second,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	id := newTestIdentity(t)
	regBody := signedMeBody(t, id, "site-A", "host-owner", time.Now().UnixMilli())
	regResp, err := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatal(err)
	}
	defer regResp.Body.Close()
	if regResp.StatusCode != 200 {
		body, _ := io.ReadAll(regResp.Body)
		t.Fatalf("register status=%d body=%q", regResp.StatusCode, body)
	}
	var out struct {
		PollSecret string `json:"poll_secret"`
	}
	if err := json.NewDecoder(regResp.Body).Decode(&out); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if out.PollSecret == "" {
		t.Fatal("register returned an empty poll_secret")
	}
	if got, _ := relay.Owners.Lookup("site-A"); got != "host-owner" {
		t.Fatalf("mapping = %q, want host-owner", got)
	}
}

// TestOwnerTunnelRoutesRemoved is the slice-6 guard: the cleartext owner HTTP
// tunnel paths (/me/<site>, /me/<site>/...) no longer exist, so the owner API +
// cookie can never traverse the relay. Hitting them is a 404 (no such route).
func TestOwnerTunnelRoutesRemoved(t *testing.T) {
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
		PollTimeout: 100 * time.Millisecond,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	for _, p := range []string{"/me/site-A", "/me/site-A/api/owner-access/whoami", "/me/site-A/owner-access/"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status=%d, want 404 (owner tunnel removed)", p, resp.StatusCode)
		}
	}
}

func TestMeRegisterRejectsEmptyFields(t *testing.T) {
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	for _, body := range []string{
		`{"site_id":""}`,
		`{"host_id":"h1"}`,
		`{"site_id":"s","host_id":"h"}`, // no public_key / sig
		`{}`,
	} {
		resp, _ := http.Post(srv.URL+"/me/register", "application/json", strings.NewReader(body))
		if resp.StatusCode != 400 {
			t.Errorf("body=%s status=%d want 400", body, resp.StatusCode)
		}
	}
}

func newSignedRelay() *Relay {
	return &Relay{Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry()}
}

// A registration whose signature does not verify against the presented key must
// be refused — otherwise anyone could claim any site.
func TestMeRegisterRejectsBadSignature(t *testing.T) {
	srv := httptest.NewServer(newSignedRelay().Handler())
	defer srv.Close()
	id := newTestIdentity(t)
	body, _ := json.Marshal(meRegisterRequest{
		SiteID: "site-A", HostID: "host-x",
		PublicKey: id.PublicKeyHex(), TsMs: time.Now().UnixMilli(),
		Sig: "00", // not a valid signature
	})
	resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad signature status=%d want 401", resp.StatusCode)
	}
}

// A registration whose signature is valid but for a DIFFERENT host_id than the
// one in the body must be refused — the signature must bind the exact mapping.
func TestMeRegisterRejectsMismatchedHostInSignature(t *testing.T) {
	srv := httptest.NewServer(newSignedRelay().Handler())
	defer srv.Close()
	id := newTestIdentity(t)
	tsMs := time.Now().UnixMilli()
	sig, _ := id.SignRawHex(tunnel.MeRegisterSigningString("site-A", "host-LEGIT", tsMs))
	body, _ := json.Marshal(meRegisterRequest{
		SiteID: "site-A", HostID: "host-ATTACKER", // body says attacker...
		PublicKey: id.PublicKeyHex(), TsMs: tsMs, Sig: sig, // ...sig covers legit
	})
	resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("host/sig mismatch status=%d want 401", resp.StatusCode)
	}
}

// A stale timestamp (outside the skew window) must be refused so a captured
// registration body can't be replayed long after the fact.
func TestMeRegisterRejectsStaleTimestamp(t *testing.T) {
	srv := httptest.NewServer(newSignedRelay().Handler())
	defer srv.Close()
	id := newTestIdentity(t)
	old := time.Now().UnixMilli() - tunnel.MeRegisterMaxSkewMs - 60_000
	body := signedMeBody(t, id, "site-A", "host-x", old)
	resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale ts status=%d want 401", resp.StatusCode)
	}
}

// The core hijack guard: once a site is pinned (TOFU on first registration), a
// registration signed by a DIFFERENT key — even a perfectly valid self-signed
// one — must be refused. This is what stops an internet attacker from
// repointing an existing site's mapping to a host they control.
func TestMeRegisterTOFUPinRejectsDifferentKey(t *testing.T) {
	srv := httptest.NewServer(newSignedRelay().Handler())
	defer srv.Close()

	owner := newTestIdentity(t)
	first := signedMeBody(t, owner, "site-A", "host-owner", time.Now().UnixMilli())
	if resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(first)); resp.StatusCode != 200 {
		t.Fatalf("first registration status=%d want 200", resp.StatusCode)
	}

	attacker := newTestIdentity(t) // different keypair, validly self-signed
	evil := signedMeBody(t, attacker, "site-A", "host-attacker", time.Now().UnixMilli())
	resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(evil))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("hijack attempt status=%d want 403", resp.StatusCode)
	}
}

// The legitimate owner re-registering (e.g. host_id changed after a reboot)
// with the SAME key must succeed and update the mapping.
func TestMeRegisterSameKeyReRegisterUpdatesHost(t *testing.T) {
	relay := newSignedRelay()
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	owner := newTestIdentity(t)
	b1 := signedMeBody(t, owner, "site-A", "host-1", time.Now().UnixMilli())
	if resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(b1)); resp.StatusCode != 200 {
		t.Fatalf("first status=%d want 200", resp.StatusCode)
	}
	b2 := signedMeBody(t, owner, "site-A", "host-2", time.Now().UnixMilli())
	if resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(b2)); resp.StatusCode != 200 {
		t.Fatalf("re-register status=%d want 200", resp.StatusCode)
	}
	if got, _ := relay.Owners.Lookup("site-A"); got != "host-2" {
		t.Fatalf("mapping = %q, want host-2 after same-key re-register", got)
	}
}

// FIX-1 (CRITICAL): /tunnel/register (unauthenticated, the friend path) must
// NEVER disclose the OWNER poll secret. An attacker who learns an owner-* host_id
// could otherwise POST a fake friend registration with it and be handed back the
// owner's poll secret minted by /me/register — then poll/inject the owner's
// /signal rendezvous. Two guards: (1) /tunnel/register rejects any owner-* host_id
// with 403, and (2) PollSecrets binds the secret to the minting principal so even
// without the prefix guard a different principal can't retrieve it.
func TestTunnelRegister_DoesNotDiscloseOwnerPollSecret(t *testing.T) {
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		Owners:      NewOwnerRegistry(),
		Polls:       NewPollSecrets(),
		PollTimeout: time.Second,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	// 1. The owner registers via the ES256-signed /me/register, minting its poll
	//    secret (bound to the site key). This is the secret the /signal rendezvous
	//    is authenticated with.
	owner := newTestIdentity(t)
	const ownerHost = "owner-home-deadbeef" // owner-* namespace, like deriveOwnerHostID
	reg := signedMeBody(t, owner, "site:Home", ownerHost, time.Now().UnixMilli())
	mr, err := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(reg))
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Body.Close()
	if mr.StatusCode != 200 {
		t.Fatalf("/me/register status=%d, want 200", mr.StatusCode)
	}
	var owned struct {
		PollSecret string `json:"poll_secret"`
	}
	if err := json.NewDecoder(mr.Body).Decode(&owned); err != nil {
		t.Fatalf("decode owner poll secret: %v", err)
	}
	if owned.PollSecret == "" {
		t.Fatal("owner /me/register returned an empty poll secret")
	}
	// Sanity: the owner secret really does authenticate the owner /signal drain.
	if !relay.Polls.Check(ownerHost, owned.PollSecret) {
		t.Fatal("owner poll secret does not authenticate its host — test premise broken")
	}

	// 2. The attacker POSTs a friend /tunnel/register using the OWNER host_id,
	//    hoping to be handed the owner's existing poll secret. It must be REFUSED
	//    (owner-* namespace reserved) — and must NOT return the owner secret.
	frBody, _ := json.Marshal(registerRequest{
		HostID: ownerHost,
		Token:  "attacker-token",
		TTLMs:  60_000,
	})
	fr, err := http.Post(srv.URL+"/tunnel/register", "application/json", bytes.NewReader(frBody))
	if err != nil {
		t.Fatal(err)
	}
	defer fr.Body.Close()
	if fr.StatusCode != http.StatusForbidden {
		t.Fatalf("/tunnel/register with owner-* host_id status=%d, want 403 (reserved)", fr.StatusCode)
	}
	var leaked registerResponse
	_ = json.NewDecoder(fr.Body).Decode(&leaked)
	if leaked.PollSecret != "" {
		t.Fatalf("/tunnel/register leaked a poll secret: %q", leaked.PollSecret)
	}
	if leaked.PollSecret == owned.PollSecret {
		t.Fatal("/tunnel/register disclosed the OWNER poll secret to the friend path")
	}
	// The owner secret is unchanged and still the only one that authenticates the
	// owner host — the attacker learned nothing.
	if !relay.Polls.Check(ownerHost, owned.PollSecret) {
		t.Fatal("owner poll secret changed after the friend-register attempt")
	}
}

// An operator-provisioned pin (Pin) is authoritative even over a first-arriving
// different key — so the internet-exposed home site can't be TOFU-claimed by a
// racer after a relay restart.
func TestMeRegisterOperatorPinBeatsTOFU(t *testing.T) {
	relay := newSignedRelay()
	owner := newTestIdentity(t)
	relay.Owners.Pin("site:Home", owner.PublicKeyHex()) // operator pins the real key
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	attacker := newTestIdentity(t)
	evil := signedMeBody(t, attacker, "site:Home", "host-attacker", time.Now().UnixMilli())
	if resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(evil)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("racer against operator pin status=%d want 403", resp.StatusCode)
	}
	// The real owner still registers fine.
	good := signedMeBody(t, owner, "site:Home", "host-owner", time.Now().UnixMilli())
	if resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(good)); resp.StatusCode != 200 {
		t.Fatalf("owner against own pin status=%d want 200", resp.StatusCode)
	}
}
