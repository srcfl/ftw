package api

// Sharing, at the HTTP door.
//
// Every route here hands out or takes away access to a house, so every one of
// them is LAN-only for the same reason the pairing QR is: somebody has to be
// inside the building. The tests below check that first and the behaviour
// second, because a share that works from the internet is not a smaller bug
// than a share that names the wrong role.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
)

func localRequest(method, target, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Host = "192.168.1.1"
	r.RemoteAddr = "192.168.1.5:1234"
	return r
}

// sessionRequest is what the passthrough hands these handlers for an owner's
// phone: a request built to look local on purpose — Host localhost, loopback
// address, no forwarding header — carrying the caller the box named when the
// session was admitted. An address alone cannot tell it from the page in the
// kitchen, which is exactly why these handlers ask the caller instead.
func sessionRequest(method, target, body string) *http.Request {
	r := localRequest(method, target, body)
	r.Host = "localhost"
	r.RemoteAddr = "127.0.0.1:0"
	return r.WithContext(apiauth.WithCaller(r.Context(), apiauth.Caller{
		Subject: apiauth.KindApp + ":aaaa1111",
		Kind:    apiauth.KindApp,
		Role:    apiauth.RoleOwner,
		Scopes:  apiauth.EveryScope(),
	}))
}

// An invite is minted for the role that was asked for, and the answer names
// that role back so the screen can say it in words above the code. A code
// whose power is invisible is the one that gets read to the wrong person.
func TestAnInviteIsMintedForTheRoleThatWasAsked(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, localRequest(
		http.MethodPost, "/api/app-link/pairing", `{"role":"viewer"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.mintedRole != "viewer" {
		t.Fatalf("minted for %q, want a viewer", enroll.mintedRole)
	}

	var got appLinkPairing
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	if got.Role != "viewer" {
		t.Fatalf("the answer says %q; the screen cannot name what it is handing out", got.Role)
	}
	if got.URL == "" {
		t.Fatal("no payload to scan")
	}
}

// A request that names no role mints nothing.
//
// It used to mint an owner, on the reasoning that a page which has not been
// updated should keep meaning what it used to mean. What that reasoning cost
// is a default that hands over a house whenever a field goes missing — and the
// field did go missing: the app sent its role in a query string this handler
// never reads, so every "invite someone to view" arrived here naming no role
// and would have left as an owner's code.
//
// A request that does not say is now asked. That is the only direction this
// particular question can safely be wrong in.
func TestAPairingRequestThatNamesNoRoleIsRefused(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	for _, body := range []string{
		"",              // no body at all
		"{}",            // a body that says nothing
		`{"kind":"qr"}`, // a body about something else
		"{not json",     // a body that will not parse
		`{"role":""}`,   // a field that arrived empty
	} {
		w := httptest.NewRecorder()
		s.handleAppLinkPairing(w, localRequest(http.MethodPost, "/api/app-link/pairing", body))

		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: got %d, want 400: %s", body, w.Code, w.Body.String())
		}
		if enroll.minted != 0 || enroll.spoken != 0 {
			t.Fatalf("body %q minted a %q code anyway", body, enroll.mintedRole)
		}
	}

	// The same request with its role named still works, or the rule above
	// would be indistinguishable from a broken endpoint. Owner mint is
	// loopback (or a house-password proof), not any LAN address.
	w := httptest.NewRecorder()
	atBox := localRequest(http.MethodPost, "/api/app-link/pairing", `{"role":"owner"}`)
	atBox.RemoteAddr = "127.0.0.1:1234"
	s.handleAppLinkPairing(w, atBox)
	if w.Code != http.StatusOK {
		t.Fatalf("naming the role got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.mintedRole != "owner" {
		t.Fatalf("minted for %q, want the owner that was asked for", enroll.mintedRole)
	}
}

// The box code comes back as characters and never as a scannable payload —
// and the QR payload never comes back as text. They are different objects with
// different handling: one is meant to be read aloud, the other must only ever
// travel through a camera.
func TestABoxCodeIsTextAndAQRCodeIsNot(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, localRequest(
		http.MethodPost, "/api/app-link/pairing", `{"role":"viewer","kind":"spoken"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.spoken != 1 || enroll.minted != 0 {
		t.Fatalf("minted %d spoken and %d scanned codes, want 1 and 0", enroll.spoken, enroll.minted)
	}

	var got appLinkPairing
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	if got.Code != "ABCD-EFGH" {
		t.Fatalf("code = %q, want the grouped characters", got.Code)
	}
	if got.URL != "" {
		t.Fatal("a spoken code came back with a scannable payload as well")
	}

	// And the other way round: the QR payload is a credential, so it must not
	// appear in the field a screen would print as text. Decoded into a fresh
	// value, because an absent field leaves whatever was there before.
	w = httptest.NewRecorder()
	atBox := localRequest(http.MethodPost, "/api/app-link/pairing", `{"role":"owner"}`)
	atBox.RemoteAddr = "127.0.0.1:1234"
	s.handleAppLinkPairing(w, atBox)
	var scanned appLinkPairing
	if err := json.Unmarshal(w.Body.Bytes(), &scanned); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	if scanned.Code != "" {
		t.Fatalf("a scanned code came back as readable text: %q", scanned.Code)
	}
	if scanned.URL == "" {
		t.Fatal("a scanned code came back with nothing to scan")
	}
}

// Every sharing surface is LAN-only, for the same reason pairing is. A code
// minted from the internet is a house handed to whoever asked.
func TestEverySharingRouteIsLocalOnly(t *testing.T) {
	remote := func(method, target, body string) *http.Request {
		r := localRequest(method, target, body)
		r.Host = "app.example.com"
		r.RemoteAddr = "203.0.113.9:1234"
		return r
	}
	proxied := func(method, target, body string) *http.Request {
		r := localRequest(method, target, body)
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		return r
	}

	for _, c := range []struct {
		name    string
		serve   func(*Server, http.ResponseWriter, *http.Request)
		method  string
		target  string
		body    string
		wrapper func(string, string, string) *http.Request
	}{
		{"invite from outside", (*Server).handleAppLinkPairing,
			http.MethodPost, "/api/app-link/pairing", `{"role":"viewer"}`, remote},
		{"invite through a proxy", (*Server).handleAppLinkPairing,
			http.MethodPost, "/api/app-link/pairing", `{"role":"viewer"}`, proxied},
		{"box code from outside", (*Server).handleAppLinkPairing,
			http.MethodPost, "/api/app-link/pairing", `{"kind":"spoken"}`, remote},
		{"box code through a proxy", (*Server).handleAppLinkPairing,
			http.MethodPost, "/api/app-link/pairing", `{"kind":"spoken"}`, proxied},
		{"role change from outside", (*Server).handleAppLinkDeviceRole,
			http.MethodPatch, "/api/app-link/devices/aaaa1111", `{"role":"viewer"}`, remote},
		{"role change through a proxy", (*Server).handleAppLinkDeviceRole,
			http.MethodPatch, "/api/app-link/devices/aaaa1111", `{"role":"viewer"}`, proxied},
	} {
		t.Run(c.name, func(t *testing.T) {
			enroll := &stubEnroller{}
			s := New(&Deps{AppEnroll: enroll})

			w := httptest.NewRecorder()
			r := c.wrapper(c.method, c.target, c.body)
			r.SetPathValue("id", "aaaa1111")
			c.serve(s, w, r)

			if w.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403 — this route changes who can reach a house", w.Code)
			}
			// Refused before anything happened. A code minted and then
			// withheld has already invalidated the one on somebody's screen.
			if enroll.minted != 0 || enroll.spoken != 0 || enroll.roleSet != "" {
				t.Fatalf("a refused request still did something: %+v", enroll)
			}
		})
	}
}

// Promoting a guest and stepping an owner down happen on the device list, in
// the same place a phone is locked out. Sharing is a role on a row.
func TestARoleIsChangedFromTheDeviceList(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	r := localRequest(http.MethodPatch, "/api/app-link/devices/aaaa1111", `{"role":"viewer"}`)
	r.SetPathValue("id", "aaaa1111")
	s.handleAppLinkDeviceRole(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.roleSet != "viewer" {
		t.Fatalf("set the role to %q, want viewer", enroll.roleSet)
	}
}

// The last owner is protected, and the refusal has to be one a person can act
// on: 409 with a sentence saying what to do first, not a 500.
//
// Removing it is refused over the session, where the household could be
// anywhere. Stepping it down is refused at either door — see the box's own
// removal test below for where the two part and why.
func TestTheLastOwnerIsRefusedWithSomethingToDo(t *testing.T) {
	for _, c := range []struct {
		name  string
		serve func(*Server, http.ResponseWriter, *http.Request)
		req   *http.Request
	}{
		{"demote at the box", (*Server).handleAppLinkDeviceRole,
			localRequest(http.MethodPatch, "/api/app-link/devices/aaaa1111", `{"role":"viewer"}`)},
		{"demote from the app", (*Server).handleAppLinkDeviceRole,
			sessionRequest(http.MethodPatch, "/api/app-link/devices/aaaa1111", `{"role":"viewer"}`)},
		{"remove from the app", (*Server).handleAppLinkDeviceRevoke,
			sessionRequest(http.MethodDelete, "/api/app-link/devices/aaaa1111", "")},
	} {
		t.Run(c.name, func(t *testing.T) {
			enroll := &stubEnroller{lastOwner: true}
			s := New(&Deps{AppEnroll: enroll})

			w := httptest.NewRecorder()
			c.req.SetPathValue("id", "aaaa1111")
			c.serve(s, w, c.req)

			if w.Code != http.StatusConflict {
				t.Fatalf("got %d, want 409: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "Pair another owner first") {
				t.Fatalf("the refusal does not say what to do: %s", w.Body.String())
			}
			// And a code beside the sentence, because two audiences read this
			// body. The box's own page prints the sentence; the app owns all
			// of its own prose and needs a name to branch on, and a 409 is a
			// conflict and nothing more specific than that. Without this the
			// app's last-owner screen is dead against a real box: it reads
			// "code" and every refusal from here sent only "error".
			var refusal struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("the refusal is not JSON: %v", err)
			}
			if refusal.Code != appproto.ErrLastOwnerProtected {
				t.Fatalf("refusal code %q, want %q — from contract/registry.yaml, "+
					"never a literal on this floor", refusal.Code, appproto.ErrLastOwnerProtected)
			}
			if enroll.revoked != 0 || enroll.roleSet != "" {
				t.Fatalf("the last owner was changed anyway: %+v", enroll)
			}
		})
	}
}

// At the box, the last phone comes off the list.
//
// The refusal above guards against a phone emptying the roster from anywhere
// in the world, leaving a box nobody can administer and no remote way to mend
// it. Whoever is standing at the box has the mend in front of them — the same
// page shows a new pairing code — so refusing there protects nothing, and it
// stranded every household that lost its only phone.
//
// The handler tells the two apart by the caller the box named, not by an
// address and not by anything in the request. A remote caller has no field to
// set that would get it here.
func TestTheLastPhoneIsRemovedAtTheBox(t *testing.T) {
	enroll := &stubEnroller{lastOwner: true}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	r := localRequest(http.MethodDelete, "/api/app-link/devices/aaaa1111", "")
	r.SetPathValue("id", "aaaa1111")
	s.handleAppLinkDeviceRevoke(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — a household at its own box cannot start clean: %s",
			w.Code, w.Body.String())
	}
	if enroll.revoked != 1 {
		t.Fatalf("the phone was answered for but never removed: %+v", enroll)
	}
	if !enroll.revokedAtTheBox {
		t.Fatal("the handler told enrolment the caller was remote; the last owner would survive a real box")
	}
}

// Removing a phone that is not the last owner is the same at both doors.
// Nothing about this change touches the ordinary case.
func TestRemovingAPhoneThatIsNotTheLastOwnerIsUnchanged(t *testing.T) {
	for _, c := range []struct {
		name string
		req  *http.Request
	}{
		{"at the box", localRequest(http.MethodDelete, "/api/app-link/devices/aaaa1111", "")},
		{"from the app", sessionRequest(http.MethodDelete, "/api/app-link/devices/aaaa1111", "")},
	} {
		t.Run(c.name, func(t *testing.T) {
			enroll := &stubEnroller{}
			s := New(&Deps{AppEnroll: enroll})

			w := httptest.NewRecorder()
			c.req.SetPathValue("id", "aaaa1111")
			s.handleAppLinkDeviceRevoke(w, c.req)

			if w.Code != http.StatusOK {
				t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
			}
			if enroll.revoked != 1 {
				t.Fatalf("the phone was answered for but never removed: %+v", enroll)
			}
		})
	}
}

// A role the registry does not define is a 400, not a 500 and not a silent
// success. The list of roles lives in contract/registry.yaml and the answer
// has to come from there rather than from a literal on this floor.
func TestAnUnknownRoleIsABadRequest(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, localRequest(
		http.MethodPost, "/api/app-link/pairing", `{"role":"administrator"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("minting for an unknown role got %d, want 400: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	r := localRequest(http.MethodPatch, "/api/app-link/devices/aaaa1111", `{"role":"administrator"}`)
	r.SetPathValue("id", "aaaa1111")
	s.handleAppLinkDeviceRole(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("setting an unknown role got %d, want 400: %s", w.Code, w.Body.String())
	}
	if enroll.roleSet != "" {
		t.Fatalf("an unknown role was written: %q", enroll.roleSet)
	}
}

// The device list carries the role, or a household cannot tell whose phone is
// a guest and cannot decide which row to remove.
func TestTheDeviceListNamesEachPhonesRole(t *testing.T) {
	s := New(&Deps{AppEnroll: &stubEnroller{}})

	w := httptest.NewRecorder()
	s.handleAppLinkDevices(w, localRequest(http.MethodGet, "/api/app-link/devices", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var body struct {
		Devices []AppDevice `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(body.Devices) != 1 || body.Devices[0].Role != "owner" {
		t.Fatalf("devices = %+v, want one row naming its role", body.Devices)
	}
	// Still no keys. The rows carry prefixes, and that has not changed.
	if strings.Contains(w.Body.String(), "noiseSecret") {
		t.Fatal("the device list leaked key material")
	}
}
