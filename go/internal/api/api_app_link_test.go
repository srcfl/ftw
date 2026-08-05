package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A stub enroller. Pairing is the one surface that hands out a credential, so
// what matters here is who is allowed to ask, not what comes back.
type stubEnroller struct {
	minted  int
	revoked int
	err     error
}

func (s *stubEnroller) MintPairingCode() ([]byte, time.Time, error) {
	if s.err != nil {
		return nil, time.Time{}, s.err
	}
	s.minted++
	return make([]byte, 16), time.Now().Add(10 * time.Minute), nil
}

func (s *stubEnroller) EnrollmentURL(code []byte, lanHint string) (string, error) {
	return "https://app.ftw.energy/p#v2.aaa.bbb.ccc.ddd", nil
}

func (s *stubEnroller) Devices() []AppDevice {
	return []AppDevice{{ID: "aaaa1111", AddedAtMs: 1, LastSeenMs: 2}}
}

func (s *stubEnroller) RevokeDevice(id string) error {
	if id != "aaaa1111" {
		return ErrUnknownAppDevice
	}
	s.revoked++
	return nil
}

func (s *stubEnroller) AuthorisedCount() int { return 2 }

func pairingRequest(host, remote string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/app-link/pairing", nil)
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

func TestPairingFromTheLAN(t *testing.T) {
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
