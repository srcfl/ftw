package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// lanAuthOnDeps is a box with the house password turned on — the state
// where owner-making from the LAN goes back behind the password proof.
func lanAuthOnDeps(enroll *stubEnroller) *Deps {
	return &Deps{
		AppEnroll: enroll,
		Cfg:       &config.Config{API: config.API{LANAuth: true}},
		CfgMu:     &sync.RWMutex{},
	}
}

// A stub enroller. Pairing is the one surface that hands out a credential, so
// what matters here is who is allowed to ask, not what comes back.
type stubEnroller struct {
	minted     int
	spoken     int
	revoked    int
	mintedRole string
	roleSet    string
	err        error
	// lastOwner makes the stub refuse to demote its one row, and to remove it
	// for anyone who is not standing at the box — the way appenroll does when
	// it is the only owner left.
	lastOwner bool
	// revokedAtTheBox records the door the last removal came through, so a
	// test can prove the handler passed the fact rather than a constant.
	revokedAtTheBox bool
	// paired, when set, is AuthorisedCount. Nil means "already has owners"
	// (2), which is what most pairing tests assume.
	paired *int
}

func (s *stubEnroller) MintPairingCode(role string) ([]byte, time.Time, error) {
	if s.err != nil {
		return nil, time.Time{}, s.err
	}
	if role != "owner" && role != "viewer" {
		return nil, time.Time{}, ErrUnknownAppRole
	}
	s.minted++
	s.mintedRole = role
	return make([]byte, 16), time.Now().Add(10 * time.Minute), nil
}

func (s *stubEnroller) MintSpokenCode(role string) (string, time.Time, error) {
	if s.err != nil {
		return "", time.Time{}, s.err
	}
	if role != "owner" && role != "viewer" {
		return "", time.Time{}, ErrUnknownAppRole
	}
	s.spoken++
	s.mintedRole = role
	return "ABCD-EFGH", time.Now().Add(5 * time.Minute), nil
}

func (s *stubEnroller) EnrollmentURL(code []byte, lanHint string) (string, error) {
	return "https://app.ftw.energy/p#v2.aaa.bbb.ccc.ddd", nil
}

func (s *stubEnroller) Devices() []AppDevice {
	return []AppDevice{{ID: "aaaa1111", AddedAtMs: 1, LastSeenMs: 2, Role: "owner"}}
}

func (s *stubEnroller) SetDeviceRole(id, role string) error {
	if id != "aaaa1111" {
		return ErrUnknownAppDevice
	}
	if role != "owner" && role != "viewer" {
		return ErrUnknownAppRole
	}
	if s.lastOwner {
		return ErrLastAppOwnerProtected
	}
	s.roleSet = role
	return nil
}

func (s *stubEnroller) RevokeDevice(id string, atTheBox bool) error {
	if id != "aaaa1111" {
		return ErrUnknownAppDevice
	}
	if s.lastOwner && !atTheBox {
		return ErrLastAppOwnerProtected
	}
	s.revoked++
	s.revokedAtTheBox = atTheBox
	return nil
}

func (s *stubEnroller) AuthorisedCount() int {
	if s.paired != nil {
		return *s.paired
	}
	return 2
}

func stubPaired(n int) *int { return &n }

// pairingRequest asks for an owner's QR code, which is what the box's own page
// asks for when somebody presses "pair my phone".
//
// The role is named rather than left out. It has to be: a request that names
// none is refused now, because a default at this endpoint decides who owns a
// house.
func pairingRequest(host, remote string, headers map[string]string) *http.Request {
	return pairingRequestRole(host, remote, headers, "owner")
}

func pairingRequestRole(host, remote string, headers map[string]string, role string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/app-link/pairing",
		strings.NewReader(`{"role":"`+role+`"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Host = host
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestPairingIsLocalOnly(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	cases := []struct {
		name    string
		host    string
		remote  string
		headers map[string]string
	}{
		{"remote host", "app.example.com", "192.168.1.5:1234", nil},
		{"remote client", "192.168.1.1", "203.0.113.9:1234", nil},
		{"behind a proxy", "192.168.1.1", "192.168.1.5:1234",
			map[string]string{"X-Forwarded-For": "203.0.113.9"}},
		{"behind a proxy, Forwarded", "192.168.1.1", "192.168.1.5:1234",
			map[string]string{"Forwarded": "for=203.0.113.9"}},
		{"behind a proxy, X-Real-IP", "192.168.1.1", "192.168.1.5:1234",
			map[string]string{"X-Real-IP": "203.0.113.9"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleAppLinkPairing(w, pairingRequest(c.host, c.remote, c.headers))

			if w.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403 — this endpoint hands out a credential", w.Code)
			}
			// The refusal must come before anything is minted: a code issued
			// and then withheld still invalidates the one on someone's screen.
			if enroll.minted != 0 {
				t.Fatalf("minted %d codes for a refused request", enroll.minted)
			}
		})
	}
}

func TestViewerPairingFromTheLAN(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, pairingRequestRole("192.168.1.1", "192.168.1.5:1234", nil, "viewer"))

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.minted != 1 {
		t.Fatalf("minted %d codes, want 1", enroll.minted)
	}
}

// Operator decision 2026-08-29: with the house password off the whole LAN
// already holds settings, control and driver install through the open
// dashboard, so refusing only owner-minting blocked the household without
// slowing an intruder. The password is the opt-in that closes this door.
func TestOwnerPairingFromTheLANWithoutHousePassword(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, pairingRequest("192.168.1.1", "192.168.1.5:1234", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.minted != 1 {
		t.Fatalf("minted %d codes, want 1", enroll.minted)
	}
}

func TestOwnerPairingFromTheLANWithHousePasswordOnIsRefused(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(lanAuthOnDeps(enroll))

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, pairingRequest("192.168.1.1", "192.168.1.5:1234", nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", w.Code, w.Body.String())
	}
	if enroll.minted != 0 {
		t.Fatalf("minted %d owner codes past the house password", enroll.minted)
	}
}

// A public client never reaches the open-LAN door, password or not.
func TestOwnerPairingFromAPublicAddressIsRefused(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, pairingRequest("192.168.1.1", "203.0.113.9:1234", nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", w.Code, w.Body.String())
	}
	if enroll.minted != 0 {
		t.Fatalf("minted %d owner codes for a public client", enroll.minted)
	}
}

func TestOwnerPairingFromLoopback(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, pairingRequest("127.0.0.1", "127.0.0.1:1234", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.minted != 1 {
		t.Fatalf("minted %d codes, want 1", enroll.minted)
	}
}

// An empty box's first pairing becomes the owner, so it follows the same
// rule: open from the LAN until a house password exists — this is also the
// onboarding path — and behind the password once one does.
func TestViewerPairingFromTheLANOnAnEmptyBoxWithoutHousePassword(t *testing.T) {
	enroll := &stubEnroller{paired: stubPaired(0)}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, pairingRequestRole("192.168.1.1", "192.168.1.5:1234", nil, "viewer"))

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.minted != 1 {
		t.Fatalf("minted %d codes, want 1", enroll.minted)
	}
}

func TestViewerPairingFromTheLANOnAnEmptyBoxWithHousePasswordOnIsRefused(t *testing.T) {
	enroll := &stubEnroller{paired: stubPaired(0)}
	s := New(lanAuthOnDeps(enroll))

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, pairingRequestRole("192.168.1.1", "192.168.1.5:1234", nil, "viewer"))

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", w.Code, w.Body.String())
	}
	if enroll.minted != 0 {
		t.Fatalf("minted %d viewer codes on an empty box past the house password", enroll.minted)
	}
}

func TestViewerPairingFromLoopbackOnAnEmptyBox(t *testing.T) {
	enroll := &stubEnroller{paired: stubPaired(0)}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, pairingRequestRole("127.0.0.1", "127.0.0.1:1234", nil, "viewer"))

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.minted != 1 {
		t.Fatalf("minted %d codes, want 1", enroll.minted)
	}
}

func ownerRolePatchRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPatch, "/api/app-link/devices/aaaa1111", strings.NewReader(`{"role":"owner"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Host = "192.168.1.1"
	r.RemoteAddr = "192.168.1.5:1234"
	r.SetPathValue("id", "aaaa1111")
	return r
}

// Promotion to owner follows the same rule as minting one: open from the
// LAN while the house password is off, behind it once it is on.
func TestOwnerRolePatchFromTheLANWithoutHousePassword(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	s.handleAppLinkDeviceRole(w, ownerRolePatchRequest())

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.roleSet != "owner" {
		t.Fatalf("role set to %q, want owner", enroll.roleSet)
	}
}

func TestOwnerRolePatchFromTheLANWithHousePasswordOnIsRefused(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(lanAuthOnDeps(enroll))

	w := httptest.NewRecorder()
	s.handleAppLinkDeviceRole(w, ownerRolePatchRequest())

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", w.Code, w.Body.String())
	}
	if enroll.roleSet != "" {
		t.Fatalf("LAN peer patched a device to owner past the house password: %q", enroll.roleSet)
	}
}

func TestOwnerRolePatchFromLoopback(t *testing.T) {
	enroll := &stubEnroller{}
	s := New(&Deps{AppEnroll: enroll})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/api/app-link/devices/aaaa1111", strings.NewReader(`{"role":"owner"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Host = "127.0.0.1"
	r.RemoteAddr = "127.0.0.1:1234"
	r.SetPathValue("id", "aaaa1111")
	s.handleAppLinkDeviceRole(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if enroll.roleSet != "owner" {
		t.Fatalf("set the role to %q, want owner", enroll.roleSet)
	}
}

func TestPairingSaysSoWhenTheAppLinkIsOff(t *testing.T) {
	// A typed nil in an interface is not nil, so this also pins that main.go
	// hands over an untyped nil rather than a disabled *appenroll.Identity.
	s := New(&Deps{})

	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, pairingRequest("192.168.1.1", "192.168.1.5:1234", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
}

func TestStatusNeverListsDevices(t *testing.T) {
	// A device list on an unauthenticated LAN endpoint is a household
	// inventory. A count answers the question a person actually has.
	s := New(&Deps{AppEnroll: &stubEnroller{}})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/app-link/status", nil)
	r.Host = "192.168.1.1"
	r.RemoteAddr = "192.168.1.5:1234"
	s.handleAppLinkStatus(w, r)

	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, body)
	}
	for _, forbidden := range []string{"devices\":[", "credential", "public_key", "pubkey"} {
		if contains(body, forbidden) {
			t.Fatalf("status leaked %q: %s", forbidden, body)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}
