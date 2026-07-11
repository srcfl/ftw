package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// register_atomicity_test.go — the FIX-A escalation guard.
//
// /tunnel/register must commit the token and its poll-secret ownership as ONE
// unit: if poll-secret ownership of the host_id can't be proven (the host_id's
// secret belongs to a different principal — an owner via /me/register, or another
// friend's pair token), the registration must leave NO token behind. Otherwise an
// attacker registers T for a victim host_id H, gets the 403, then approves T with
// their own code and /h/T/{mcp,web} routes friend traffic to H — unauthorized
// access to the victim's Pi.

// postRegister is a small helper that POSTs a /tunnel/register body and returns
// the status code.
func postRegister(t *testing.T, srv *httptest.Server, hostID, token string) int {
	t.Helper()
	body, _ := json.Marshal(registerRequest{HostID: hostID, Token: token, TTLMs: 3_600_000, ApprovalCode: "4827"})
	resp, err := http.Post(srv.URL+"/tunnel/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register POST: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestRegister_RejectsOwnerHostID_NoTokenLeft proves a /tunnel/register for an
// owner-namespaced host_id (the reserved "owner-" prefix) is refused AND leaves
// no token in the registry — so the friend can't approve it to reach an owner Pi.
func TestRegister_RejectsOwnerHostID_NoTokenLeft(t *testing.T) {
	r := newTestRelay()
	r.Polls = NewPollSecrets()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	const tok = "sneaky-token"
	if code := postRegister(t, srv, "owner-Home-abcd", tok); code != http.StatusForbidden {
		t.Fatalf("register for owner host_id status = %d, want 403", code)
	}
	if _, err := r.Tokens.Get(tok); err == nil {
		t.Fatal("FIX-A: a 403 for an owner host_id left a token registered — friend could approve it and reach the owner Pi")
	}
}

// TestRegister_PrincipalMismatch_NoTokenLeft is the atomicity core: a host_id
// whose poll secret is already bound to ANOTHER principal (here a live owner key,
// minted exactly as /me/register does — but the bug also covers a non-owner
// host_id held by a different friend). The register must 403 AND leave no token.
// Then we prove an attacker who got the 403 cannot approve the token (it is gone)
// and therefore cannot route /h/<token>/mcp to the victim host_id.
func TestRegister_PrincipalMismatch_NoTokenLeft(t *testing.T) {
	r := newTestRelay()
	r.Polls = NewPollSecrets()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	// A live owner already owns the poll secret for this host_id, bound to its
	// site key principal (this is what /me/register does). Use a NON-"owner-"
	// host_id so the owner-prefix guard does NOT short-circuit — this exercises
	// the atomicity path (token registered, then Issue rejects).
	const victimHost = "victim-host-12ab"
	const attackerTok = "attacker-token"
	if _, err := r.Polls.Issue(victimHost, "site:VICTIM-OWNER-KEY"); err != nil {
		t.Fatalf("seed victim poll secret: %v", err)
	}

	// Attacker registers a token for the victim's host_id. The token is created,
	// then poll-secret ownership fails (different principal) → must be rolled back.
	if code := postRegister(t, srv, victimHost, attackerTok); code != http.StatusForbidden {
		t.Fatalf("register against a foreign-principal host_id status = %d, want 403", code)
	}

	// THE INVARIANT: no token remains. A leaked token here is the escalation.
	if _, err := r.Tokens.Get(attackerTok); err == nil {
		t.Fatal("FIX-A: principal-mismatch 403 left a token registered — the registration was NOT atomic")
	}

	// And the victim's poll secret must be untouched (rollback released only the
	// token, never the owner's secret).
	if !r.Polls.Check(victimHost, mustOwnerSecret(t, r.Polls, victimHost)) {
		t.Fatal("victim poll secret was disturbed by the failed register")
	}

	// End-to-end: the attacker can't now approve the (absent) token, so /h/<token>
	// can't be used to reach the victim host. Approve → token-not-found → 403/404,
	// and /h/<token>/mcp is likewise dead.
	ar, _ := http.Post(srv.URL+"/h/"+attackerTok+"/approve", "application/json",
		bytes.NewReader([]byte(`{"code":"4827"}`)))
	ar.Body.Close()
	if ar.StatusCode == http.StatusOK {
		t.Fatalf("attacker approved a token that should not exist (status %d)", ar.StatusCode)
	}
}

// mustOwnerSecret re-derives the owner's stable secret (Issue is idempotent for
// the same principal) so the test can assert it still verifies.
func mustOwnerSecret(t *testing.T, p *PollSecrets, hostID string) string {
	t.Helper()
	s, err := p.Issue(hostID, "site:VICTIM-OWNER-KEY")
	if err != nil {
		t.Fatalf("re-issue owner secret: %v", err)
	}
	return s
}

// TestRegister_HappyPath_StillWorks is a guard that the atomicity rewrite didn't
// break the normal friend register: a fresh host_id registers, returns a poll
// secret, and the token is present.
func TestRegister_HappyPath_StillWorks(t *testing.T) {
	r := newTestRelay()
	r.Polls = NewPollSecrets()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	body, _ := json.Marshal(registerRequest{HostID: "friend-host-1", Token: "good-token", TTLMs: 3_600_000, ApprovalCode: "1234"})
	resp, err := http.Post(srv.URL+"/tunnel/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("happy-path register status = %d, want 200", resp.StatusCode)
	}
	var out registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.PollSecret == "" {
		t.Fatal("happy-path register returned no poll secret")
	}
	if _, err := r.Tokens.Get("good-token"); err != nil {
		t.Fatalf("happy-path token missing: %v", err)
	}
	// The friend's poll secret authenticates its own polls.
	if !r.Polls.Check("friend-host-1", out.PollSecret) {
		t.Fatal("friend poll secret does not verify")
	}
}

// TestRegister_LiveOwnerViaMeRegister_NoTokenLeft is the literal escalation
// scenario end to end: an owner registers via the ES256-signed /me/register
// (live owner host_id, owner-prefixed), then an attacker tries /tunnel/register
// for that exact host_id. It must 403 and leave no token, so the owner's signal
// rendezvous / Pi can never be reached through a friend grant.
func TestRegister_LiveOwnerViaMeRegister_NoTokenLeft(t *testing.T) {
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		Owners:      NewOwnerRegistry(),
		Polls:       NewPollSecrets(),
		Signals:     NewSignalMailbox(),
		PollTimeout: time.Second,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	id := newTestIdentity(t)
	ownerHost := "owner-Home-deadbeef"
	regBody := signedMeBody(t, id, "site:Home", ownerHost, time.Now().UnixMilli())
	mr, err := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatalf("me/register: %v", err)
	}
	mr.Body.Close()
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("me/register status = %d, want 200", mr.StatusCode)
	}

	const attackerTok = "friend-grab"
	if code := postRegister(t, srv, ownerHost, attackerTok); code != http.StatusForbidden {
		t.Fatalf("attacker register for live owner host_id status = %d, want 403", code)
	}
	if _, err := relay.Tokens.Get(attackerTok); err == nil {
		t.Fatal("FIX-A: 403 for a live owner host_id still left a token registered")
	}
}
