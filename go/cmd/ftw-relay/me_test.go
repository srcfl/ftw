package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/nova"
	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
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

// TestMeRegisterAndForward stands up a relay + a fake host running
// the tunnel loop, registers the site, and confirms /me/<site>/x lands
// at /x on the host.
func TestMeRegisterAndForward(t *testing.T) {
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		Owners:      NewOwnerRegistry(),
		PollTimeout: 1 * time.Second,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	// Local handler the host will forward to.
	hostBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Inner-Path", r.URL.Path)
		_, _ = w.Write([]byte("hello from host:" + r.URL.Path))
	})

	host := tunnel.NewHost(srv.URL, "host-owner", hostBackend)
	host.PollTimeout = 1 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)

	// 1. Register the site → host mapping (ES256-signed).
	id := newTestIdentity(t)
	regBody := signedMeBody(t, id, "site-A", "host-owner", time.Now().UnixMilli())
	regResp, err := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatal(err)
	}
	if regResp.StatusCode != 204 {
		body, _ := io.ReadAll(regResp.Body)
		t.Fatalf("register status=%d body=%q", regResp.StatusCode, body)
	}

	// 2. /me/<site>/ → host sees /owner-access/
	r1, err := http.Get(srv.URL + "/me/site-A")
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := io.ReadAll(r1.Body)
	r1.Body.Close()
	if r1.StatusCode != 200 {
		t.Fatalf("/me/<site> status=%d body=%q", r1.StatusCode, b1)
	}
	if !strings.Contains(string(b1), "/owner-access/") {
		t.Fatalf("expected /owner-access/ inner path, got %q", b1)
	}
	if got := r1.Header.Get("X-Inner-Path"); got != "/owner-access/" {
		t.Fatalf("inner path header = %q want /owner-access/", got)
	}

	// 3. /me/<site>/api/owner-access/whoami → host sees /api/owner-access/whoami
	r2, err := http.Get(srv.URL + "/me/site-A/api/owner-access/whoami")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("/api/owner-access/whoami status=%d body=%q", r2.StatusCode, b2)
	}
	if got := r2.Header.Get("X-Inner-Path"); got != "/api/owner-access/whoami" {
		t.Fatalf("inner path = %q", got)
	}
}

func TestMeUnknownSiteReturns503(t *testing.T) {
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
		PollTimeout: 100 * time.Millisecond,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/me/unknown")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
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
	if resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(first)); resp.StatusCode != 204 {
		t.Fatalf("first registration status=%d want 204", resp.StatusCode)
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
	if resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(b1)); resp.StatusCode != 204 {
		t.Fatalf("first status=%d want 204", resp.StatusCode)
	}
	b2 := signedMeBody(t, owner, "site-A", "host-2", time.Now().UnixMilli())
	if resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(b2)); resp.StatusCode != 204 {
		t.Fatalf("re-register status=%d want 204", resp.StatusCode)
	}
	if got, _ := relay.Owners.Lookup("site-A"); got != "host-2" {
		t.Fatalf("mapping = %q, want host-2 after same-key re-register", got)
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
	if resp, _ := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(good)); resp.StatusCode != 204 {
		t.Fatalf("owner against own pin status=%d want 204", resp.StatusCode)
	}
}
